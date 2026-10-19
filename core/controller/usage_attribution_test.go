//nolint:testpackage // Exercises the unexported handlers and harnesses.
package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/consume"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	relaycontroller "github.com/labring/aiproxy/core/relay/controller"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func imageTaskInfo(t *testing.T, h *imageCallLogHarness) model.AsyncUsageInfo {
	t.Helper()

	var info model.AsyncUsageInfo
	require.NoError(t, h.db.Where("image_task_id = ?", h.requestID).First(&info).Error)

	return info
}

// Key usage P2: an image task's call row, outbox and wallet hold carry one
// identity whose time is the request start. Old: the row took the later
// handler time and nothing carried attribution.
func TestImageTaskReservationUsesRequestStart(t *testing.T) {
	h := newImageCallLogHarness(t, http.StatusOK)
	h.requestStart = time.Now().Add(-2 * time.Second).Truncate(time.Microsecond)

	w := h.submit()
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	logs := h.logs(t)
	require.Len(t, logs, 1)
	row := logs[0]
	start := model.UsageTime(h.requestStart)
	require.Equal(t, start, row.RequestAt.UTC(), "the call row's time is the request start")

	var task model.ImageTask
	require.NoError(t, h.db.First(&task, "id = ?", h.requestID).Error)
	require.NotEmpty(t, task.BillingOperationID)
	require.Equal(t, model.EmptyNullString(task.BillingOperationID), row.BillingOperationID)

	info := imageTaskInfo(t, h)
	usage, ok := info.UsageIdentity()
	require.True(t, ok, "a P2 info carries its usage columns")
	require.Equal(t, row.UsageIdentity(), usage)
	require.Equal(t, start.UnixMilli(), int64(info.UsageAtMs))
	require.True(
		t,
		info.RequestAt.After(h.requestStart),
		"info.request_at stays the handler's pricing instant",
	)

	require.NotEmpty(t, h.commands)
	admit := h.commands[0]
	require.Equal(t, "admit", admit["action"])
	require.Equal(t, task.BillingOperationID, admit["billingOperationId"])
	require.EqualValues(t, row.TokenID, admit["apiKeyId"])
	require.Equal(t, usage.ModelID, admit["usageModelId"])
	require.Equal(t, usage.Source, admit["source"])
	require.EqualValues(t, start.UnixMilli(), admit["usageAtMs"])
	require.Equal(t, task.Model, admit["modelId"], "modelId keeps the registry entry id")
}

// Plan C8: the image task keeps pricing at handler time, exactly as before.
// A conditional price whose window starts between the request start and the
// handler time is still selected; only the usage time is the request start.
// A version that priced at usage_at_ms would select the out-of-window price.
func TestImageTaskPricingKeepsHandlerTime(t *testing.T) {
	windowStart := time.Now().Truncate(time.Second)
	price := model.Price{
		PerRequestPrice: 1,
		ConditionalPrices: []model.ConditionalPrice{{
			Condition: model.PriceCondition{StartTime: windowStart.Unix()},
			Price:     model.Price{PerRequestPrice: 5},
		}},
	}

	h := newImageHarness(t, imageHarnessOptions{unprepaid: true, price: price})
	h.requestStart = windowStart.Add(-2 * time.Second)

	w := h.submit()
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	info := imageTaskInfo(t, h)
	require.False(
		t,
		info.RequestAt.Before(windowStart),
		"info.request_at is the handler time, inside the window",
	)
	require.Equal(t, h.requestStart.UnixMilli(), int64(info.UsageAtMs))

	logs := h.logs(t)
	require.Len(t, logs, 1)
	require.Equal(t, model.UsageTime(h.requestStart), logs[0].RequestAt.UTC())

	usage := model.Usage{ImageOutputTokens: 1}
	charged := consume.CalculateAmountDetailWithOptions(
		http.StatusOK,
		usage,
		info.UsageContext,
		info.Price,
		model.PriceSelectionOptions{RequestAt: info.RequestAt},
	)
	require.InDelta(
		t,
		5,
		charged.UsedAmount,
		1e-9,
		"the outbox prices at info.request_at: the in-window price",
	)

	atUsage := consume.CalculateAmountDetailWithOptions(
		http.StatusOK,
		usage,
		info.UsageContext,
		info.Price,
		model.PriceSelectionOptions{RequestAt: time.UnixMilli(int64(info.UsageAtMs))},
	)
	require.InDelta(
		t,
		1,
		atUsage.UsedAmount,
		1e-9,
		"pricing at the usage time would change the amount",
	)
}

// Plan C10: a prepaid image task refused by the wallet whose reservation
// cannot be released keeps one row that drops its op and stays rejected; the
// wallet saw only the refused admit, so it holds nothing. With the op rule but
// without C10 the row would read failed.
func TestPrepaidImageRefusalReleaseFailed(t *testing.T) {
	h := newImageCallLogHarness(t, http.StatusPaymentRequired)

	oldRelease := releaseImageTaskReservation
	releaseImageTaskReservation = func(string) error { return errTestReleaseUnavailable }
	t.Cleanup(func() { releaseImageTaskReservation = oldRelease })

	w := h.submit()
	require.Equal(t, http.StatusPaymentRequired, w.Code, w.Body.String())

	logs := h.logs(t)
	require.Len(t, logs, 1)
	require.Equal(t, http.StatusPaymentRequired, logs[0].Code)
	require.Equal(t, model.FailureStageBalance, logs[0].FailureStage)
	require.Empty(t, logs[0].BillingOperationID)
	require.Equal(t, model.CallStatusRejected, logs[0].CallStatus(time.Now()))

	var nulls int64
	require.NoError(
		t,
		h.db.Model(&model.Log{}).Where("billing_operation_id IS NULL").Count(&nulls).Error,
	)
	require.EqualValues(t, 1, nulls)

	require.Len(
		t,
		h.commands,
		1,
		"the wallet only saw the refused admit: no hold, no refund, no debit",
	)
	require.Equal(t, "admit", h.commands[0]["action"])
}

type testReleaseError string

func (e testReleaseError) Error() string { return string(e) }

const errTestReleaseUnavailable = testReleaseError("release unavailable")

func relayAttributionDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "relay-attribution.db"))
	require.NoError(t, err)
	require.NoError(
		t,
		db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}, &model.RequestDetail{}),
	)

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	return db
}

func relayAttributionContext(t *testing.T, path string) *gin.Context {
	t.Helper()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, nil)
	c.Set(middleware.GroupBalance, &middleware.GroupBalanceConsumer{})
	c.Set(
		middleware.Token,
		model.TokenCache{ID: 7, Name: model.PlaygroundCredentialName, Group: "u_1"},
	)
	c.Set(middleware.PublicModel, "vendor/model")
	c.Set(middleware.ResolvedCapability, "text-to-video")

	return c
}

// G2 for the relay: the measured image row and its outbox, and the video row
// and its outbox, carry the meta op and one identity; a failed read (poll or
// download) never carries an op. Old: no op column and no usage columns.
func TestRelayRowsAndOutboxesShareOneIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	requestAt := time.Date(2026, time.October, 12, 1, 2, 3, 456_789_000, time.UTC)
	newMeta := func(requestID string, relayMode mode.Mode) *meta.Meta {
		return &meta.Meta{
			RequestID:          requestID,
			BillingOperationID: "op-" + requestID,
			RequestAt:          requestAt,
			Mode:               relayMode,
			OriginModel:        "upstream-model",
			Group: model.GroupCache{
				ID: "u_1",
			},
			Token: model.TokenCache{ID: 7, Name: model.PlaygroundCredentialName},
		}
	}

	t.Run("measured image", func(t *testing.T) {
		db := relayAttributionDB(t)
		m := newMeta("measured", mode.ImagesGenerations)
		price := model.Price{
			OutputPrice:     1,
			OutputPriceUnit: 1,
			ImageBilling:    &model.ImageBillingPolicy{Version: 1, Scenario: "generation"},
		}
		recordResult(
			relayAttributionContext(t, "/v1/images/generations"),
			m,
			price,
			&relaycontroller.HandleResult{},
			0,
			true,
			nil,
		)

		var row model.Log
		require.NoError(t, db.Where("request_id = ?", "measured").First(&row).Error)
		require.Equal(t, model.EmptyNullString("op-measured"), row.BillingOperationID)

		var info model.AsyncUsageInfo
		require.NoError(t, db.Where("request_id = ?", "measured").First(&info).Error)
		usage, ok := info.UsageIdentity()
		require.True(t, ok)
		require.Equal(t, row.UsageIdentity(), usage)
		require.Equal(t, "vendor/model/text-to-video", usage.ModelID)
		require.Equal(t, model.RequestSourcePlayground, usage.Source)
		require.Equal(t, model.UsageTime(requestAt), usage.UsageAt)
		require.Equal(t, "op-measured", info.BillingOperationID)
	})

	t.Run("video outbox", func(t *testing.T) {
		db := relayAttributionDB(t)
		m := newMeta("video", mode.Videos)
		recordResult(
			relayAttributionContext(t, "/v1/videos"),
			m,
			model.Price{},
			&relaycontroller.HandleResult{
				AsyncUsage: true,
				UpstreamID: "upstream-video",
			},
			0,
			true,
			nil,
		)
		consume.Wait()

		var row model.Log
		require.Eventually(t, func() bool {
			return db.Where("request_id = ?", "video").First(&row).Error == nil
		}, 5*time.Second, 10*time.Millisecond)
		require.Equal(t, model.EmptyNullString("op-video"), row.BillingOperationID)

		var info model.AsyncUsageInfo
		require.NoError(t, db.Where("request_id = ?", "video").First(&info).Error)
		usage, ok := info.UsageIdentity()
		require.True(t, ok)
		require.Equal(t, row.UsageIdentity(), usage)
		require.Equal(t, consume.RelayUsageIdentity(m), usage)
	})

	t.Run("failed read has no op", func(t *testing.T) {
		db := relayAttributionDB(t)
		m := newMeta("poll", mode.VideosGet)
		result := &relaycontroller.HandleResult{
			Error: relaymodel.WrapperOpenAIError(
				errors.New("gone"),
				"upstream_failed",
				http.StatusBadGateway,
			),
		}
		recordResult(
			relayAttributionContext(t, "/v1/videos/x"),
			m,
			model.Price{},
			result,
			0,
			true,
			nil,
		)
		consume.Wait()

		var row model.Log
		require.Eventually(t, func() bool {
			return db.Where("request_id = ?", "poll").First(&row).Error == nil
		}, 5*time.Second, 10*time.Millisecond)
		require.Empty(t, row.BillingOperationID, "reads never move money")
	})
}
