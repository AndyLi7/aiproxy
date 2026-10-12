//nolint:testpackage
package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const imageCallLogRequestID = "call-log"

type imageCallLogHarness struct {
	db            *gorm.DB
	providerCalls int
	requestID     string
	submit        func() *httptest.ResponseRecorder
}

// newImageCallLogHarness runs submitImageTask behind the operational log
// middleware, against a prepaid fal channel and a fake wallet whose admit
// answer is admitStatus.
func newImageCallLogHarness(t *testing.T, admitStatus int) *imageCallLogHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	h := &imageCallLogHarness{requestID: imageCallLogRequestID}

	wallet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var command balance.PrepaymentCommand
		require.NoError(t, json.NewDecoder(r.Body).Decode(&command))

		if command.Action == "admit" && admitStatus != http.StatusOK {
			w.WriteHeader(admitStatus)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 1, "message": "refused"})

			return
		}

		receipt := map[string]any{"id": command.BillingOperationID, "status": "pending", "prepaidMicros": 2000000}
		if command.Action == "begin_attempt" {
			receipt["claimed"] = true
		}

		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": receipt}))
	}))
	t.Cleanup(wallet.Close)

	oldBalance := balance.Default
	balance.Default = balance.NewExternalHTTP(wallet.URL, "local-only")
	t.Cleanup(func() { balance.Default = oldBalance })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h.providerCalls++
		_, _ = w.Write([]byte(`{"request_id":"upstream"}`))
	}))
	t.Cleanup(upstream.Close)

	raw, err := os.ReadFile("../common/registryvalidation/testdata/provider.json")
	require.NoError(t, err)

	var contract map[string]any
	require.NoError(t, json.Unmarshal(raw, &contract))
	contract["execution"] = map[string]any{"mode": "async", "output": "image"}

	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "call-log.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}, &model.RequestDetail{}))

	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = oldLogDB })

	h.db = db

	channel := &model.Channel{
		ID: 7, Type: model.ChannelTypeFal, Key: "secret", BaseURL: upstream.URL,
		Models: []string{"image"}, ModelMapping: map[string]string{"image": "fal-ai/test"},
		Configs: map[string]any{providerBindingsConfig: map[string]any{"image": map[string]any{
			"provider": "small", "id": "small", "revision": "1",
			"contractHash": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}}},
	}

	var quote map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{"version":1,"quoteVersion":"test-1","currency":"USD","prepaidMicros":2000000,"routes":[{"routeId":"primary","channelId":7,"provider":"fal","endpoint":"fal-ai/test","credentialScope":"channel-7","estimatedMicros":1000000,"prepaidMicros":2000000,"rule":{"mode":"list_ratio","ratio":"1"}}]}`), &quote))

	engine := gin.New()
	engine.Use(middleware.OperationalLogMiddleware())
	engine.POST("/v1/images/tasks", func(c *gin.Context) {
		// An internal group may pin the channel with the Aiproxy-Channel header.
		c.Set(middleware.Group, model.GroupCache{ID: "g", Status: model.GroupStatusInternal})
		c.Set(middleware.Token, model.TokenCache{ID: 1, Name: "prod", Group: "g"})
		c.Set(middleware.RequestModel, "image")
		c.Set(middleware.RoutingModel, "image")
		c.Set(middleware.RequestedModel, "image")
		c.Set(middleware.ModelCaches, &model.ModelCaches{ChannelsByID: map[int]*model.Channel{7: channel}})
		c.Set(middleware.ModelConfig, model.ModelConfig{
			Price: model.Price{PerRequestPrice: 1},
			Config: map[model.ModelConfigKey]any{
				"x_token_platform_capability_contract": map[string]any{"entry_id": "image", "contract": contract},
				"x_token_platform_pricing":             map[string]any{"currency": "USD", "pricing_version": "v1"},
				model.ImagePrepaymentConfigKey:         quote,
			},
		})
		submitImageTask(c)
	})

	h.submit = func() *httptest.ResponseRecorder {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/images/tasks",
			bytes.NewBufferString(`{"model":"image","prompt":"hi","n":1}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Request-ID", h.requestID)
		request.Header.Set(AIProxyChannelHeader, "7")

		w := httptest.NewRecorder()
		engine.ServeHTTP(w, request)

		return w
	}

	return h
}

func (h *imageCallLogHarness) logs(t *testing.T) []model.Log {
	t.Helper()

	var logs []model.Log
	require.NoError(t, h.db.Order("id").Find(&logs).Error)

	return logs
}

// An ambiguous wallet reply after the reservation used to leave the 202 row
// and add a middleware row under an image-trace-* id: one request, two calls.
func TestImageTaskPrepaymentPendingUpdatesTheReservationRow(t *testing.T) {
	h := newImageCallLogHarness(t, http.StatusInternalServerError)

	w := h.submit()
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "prepayment_pending")
	require.Zero(t, h.providerCalls)

	logs := h.logs(t)
	require.Len(t, logs, 1, "one HTTP request must keep one log row")
	require.Equal(t, imageCallLogRequestID, logs[0].RequestID.String())
	require.Equal(t, http.StatusServiceUnavailable, logs[0].Code)
	require.Equal(t, "prepayment_pending", logs[0].ErrorCode)
	require.Equal(t, model.FailureStageNone, logs[0].FailureStage)
	require.Equal(t, model.CallStatusFailed, logs[0].CallStatus(time.Now()))
	require.True(t, logs[0].IsCall())
}

// A wallet refusal that releases the reservation leaves only the middleware's
// rejection row, as for any gate refusal.
func TestImageTaskReleasedRefusalKeepsOneRejectionRow(t *testing.T) {
	h := newImageCallLogHarness(t, http.StatusPaymentRequired)

	w := h.submit()
	require.Equal(t, http.StatusPaymentRequired, w.Code, w.Body.String())

	logs := h.logs(t)
	require.Len(t, logs, 1)
	require.Equal(t, http.StatusPaymentRequired, logs[0].Code)
	require.Equal(t, model.FailureStageBalance, logs[0].FailureStage)
	require.Equal(t, model.CallStatusRejected, logs[0].CallStatus(time.Now()))
	require.Equal(t, 1, logs[0].TokenID)
	require.Equal(t, "g", logs[0].GroupID)

	var tasks int64
	require.NoError(t, h.db.Model(&model.ImageTask{}).Count(&tasks).Error)
	require.Zero(t, tasks)
}

// When the release fails the task is failed in place; the reservation row
// records the refusal the customer received, still as a rejection.
func TestImageTaskUnreleasedRefusalUpdatesTheReservationRow(t *testing.T) {
	h := newImageCallLogHarness(t, http.StatusPaymentRequired)

	oldRelease := releaseImageTaskReservation
	releaseImageTaskReservation = func(string) error { return errors.New("release unavailable") }
	t.Cleanup(func() { releaseImageTaskReservation = oldRelease })

	w := h.submit()
	require.Equal(t, http.StatusPaymentRequired, w.Code, w.Body.String())

	logs := h.logs(t)
	require.Len(t, logs, 1)
	require.Equal(t, imageCallLogRequestID, logs[0].RequestID.String())
	require.Equal(t, http.StatusPaymentRequired, logs[0].Code)
	require.Equal(t, model.FailureStageBalance, logs[0].FailureStage)
	require.Equal(t, "insufficient_balance", logs[0].ErrorCode)
	require.Equal(t, model.CallStatusRejected, logs[0].CallStatus(time.Now()))
}

// A store failure after reservation is ours: the row records it as failed.
func TestImageTaskStoreFailureAfterReservationUpdatesTheRow(t *testing.T) {
	h := newImageCallLogHarness(t, http.StatusOK)

	require.NoError(t, h.db.Callback().Update().Before("gorm:update").Register(
		"test:fail_image_attempt_save",
		func(tx *gorm.DB) {
			values, ok := tx.Statement.Dest.(map[string]any)
			if !ok || tx.Statement.Table != "image_tasks" {
				return
			}

			if _, attempts := values["attempts"]; attempts {
				_ = tx.AddError(errors.New("forced attempt save failure"))
			}
		},
	))

	w := h.submit()
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "task_store_unavailable")
	require.Zero(t, h.providerCalls)

	logs := h.logs(t)
	require.Len(t, logs, 1)
	require.Equal(t, imageCallLogRequestID, logs[0].RequestID.String())
	require.Equal(t, http.StatusServiceUnavailable, logs[0].Code)
	require.Equal(t, "task_store_unavailable", logs[0].ErrorCode)
	require.Equal(t, model.CallStatusFailed, logs[0].CallStatus(time.Now()))
}

func TestImageTaskAcceptedKeepsTheReservationRow(t *testing.T) {
	h := newImageCallLogHarness(t, http.StatusOK)

	w := h.submit()
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	require.Equal(t, 1, h.providerCalls)

	logs := h.logs(t)
	require.Len(t, logs, 1)
	require.Equal(t, http.StatusAccepted, logs[0].Code)
	require.Equal(t, model.FailureStageNone, logs[0].FailureStage)
	require.Empty(t, logs[0].ErrorCode)
	require.Equal(t, model.CallStatusProcessing, logs[0].CallStatus(time.Now()))
}

// The guard reads the status from the writer, so an error branch added later
// is covered without its own update; if the update fails the middleware still
// writes a row, so the call is never lost.
func TestFinishReservedImageCall(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "finish.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.RequestDetail{}))

	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = oldLogDB })

	reservation := &model.Log{
		RequestID: "reserved", GroupID: "g", TokenID: 1, Code: http.StatusAccepted,
		AsyncUsageStatus: model.AsyncUsageStatusPending, Endpoint: "/v1/images/tasks",
	}
	require.NoError(t, db.Create(reservation).Error)

	engine := gin.New()
	engine.Use(middleware.OperationalLogMiddleware())
	engine.POST("/direct/:log", func(c *gin.Context) {
		middleware.MarkOperationalLogRecorded(c)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "boom"})

		logID := reservation.ID
		if c.Param("log") == "missing" {
			logID = reservation.ID + 1000
		}

		finishReservedImageCall(c, logID, "reserved", "g", false)
	})

	for _, path := range []string{"/direct/existing", "/direct/missing"} {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, nil))
		require.Equal(t, http.StatusInternalServerError, w.Code)
	}

	var updated model.Log
	require.NoError(t, db.First(&updated, reservation.ID).Error)
	require.Equal(t, http.StatusInternalServerError, updated.Code)
	require.Equal(t, model.CallStatusFailed, updated.CallStatus(time.Now()))

	// The failed update handed the row back to the middleware: a fallback row.
	var count int64
	require.NoError(t, db.Model(&model.Log{}).Count(&count).Error)
	require.EqualValues(t, 2, count)
}
