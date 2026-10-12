//nolint:testpackage // Exercises nativeHTTP's wiring behind the real middleware.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const (
	attributionNativeContract = `{"version":1,"model":"brand/model/native","input_schema":{"type":"object","required":["seed"],"properties":{"seed":{"type":"integer"}},"additionalProperties":false},"output_schema":{"type":"object"}}`
	attributionNativeQuote    = `{"version":1,"currency":"USD","quoteVersion":"q","prepaidMicros":200,"routes":[{"routeId":"r","channelId":1,"provider":"fal","endpoint":"fal-ai/vector/model","credentialScope":"scope","quantityMetric":"request","estimatedMicros":100,"prepaidMicros":200,"rule":{"mode":"cost_markup","ratio":"1"}}]}`
	attributionNativeBody     = `{"model":"brand/model/native","input":{"seed":1}}`
)

// nativeAttributionWallet behaves like the application wallet for one
// operation: the first admission's time and source are the hold's, returned on
// every receipt, including admit replays.
type nativeAttributionWallet struct {
	balance.GroupBalance

	mu        sync.Mutex
	commands  []balance.PrepaymentCommand
	holdAt    map[string]int64
	holdFrom  map[string]string
	admitErr  error
	beginErr  error
	unclaimed bool
	onBegin   func()
}

func (w *nativeAttributionWallet) Prepayment(_ context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.commands = append(w.commands, c)
	receipt := balance.PrepaymentReceipt{ID: c.BillingOperationID, Status: "pending", PrepaidMicros: 200}

	switch c.Action {
	case "admit":
		if w.admitErr != nil {
			return balance.PrepaymentReceipt{}, w.admitErr
		}

		if _, held := w.holdAt[c.BillingOperationID]; !held && c.UsageAtMs != nil {
			w.holdAt[c.BillingOperationID] = *c.UsageAtMs
			w.holdFrom[c.BillingOperationID] = c.Source
		}
	case "begin_attempt":
		if w.beginErr != nil {
			return balance.PrepaymentReceipt{}, w.beginErr
		}

		if w.onBegin != nil {
			w.onBegin()
		}

		receipt.Claimed = !w.unclaimed
	}

	if at, held := w.holdAt[c.BillingOperationID]; held {
		receipt.UsageAtMs = &at
		receipt.UsageSource = w.holdFrom[c.BillingOperationID]
	}

	return receipt, nil
}

type nativeAttributionProvider struct {
	submit func() (string, error)
}

func (p nativeAttributionProvider) SubmitNative(context.Context, string, []byte) (string, error) {
	if p.submit == nil {
		return "upstream-1", nil
	}

	return p.submit()
}

type nativeAttributionHarness struct {
	db       *gorm.DB
	wallet   *nativeAttributionWallet
	provider nativeAttributionProvider
	start    time.Time
}

func newNativeAttributionHarness(t *testing.T) *nativeAttributionHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "native-attribution.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}, &model.Log{}, &model.RequestDetail{}))

	h := &nativeAttributionHarness{
		db:     db,
		wallet: &nativeAttributionWallet{holdAt: map[string]int64{}, holdFrom: map[string]string{}},
	}

	oldDB, oldWallet := model.LogDB, balance.Default
	model.LogDB, balance.Default = db, h.wallet
	t.Cleanup(func() {
		model.LogDB, balance.Default = oldDB, oldWallet

		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	return h
}

// submit runs one POST /v1/model-tasks through the operational log middleware
// and the controller's nativeHTTP wiring, with the website Playground's
// hidden credential.
func (h *nativeAttributionHarness) submit(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()

	router := gin.New()
	router.Use(func(c *gin.Context) {
		middleware.SetRequestAt(c, h.start)
		middleware.SetRequestID(c, id)
		c.Next()
	})
	router.Use(middleware.OperationalLogMiddleware())
	router.POST("/v1/model-tasks", func(c *gin.Context) {
		c.Set(middleware.Group, model.GroupCache{ID: "u_1", Status: model.GroupStatusEnabled})
		c.Set(middleware.Token, model.TokenCache{ID: 1, Name: model.PlaygroundCredentialName, Group: "u_1"})

		handler, ok := nativeHTTP(c)
		require.True(t, ok)

		handler.ResolvePlan = func(*http.Request, []byte) (nativetask.Plan, nativetask.Provider, error) {
			return nativetask.Plan{
				Contract: json.RawMessage(attributionNativeContract), ChannelID: 1, Endpoint: "fal-ai/vector/model",
				CredentialScope: "scope", KeyFingerprint: model.ImageChannelKeyFingerprint("test-native-key"),
				QuoteJSON: attributionNativeQuote, Log: nativeTaskLogInfo(c),
			}, h.provider, nil
		}
		handler.Create(c.Writer, c.Request)
	})

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/model-tasks", strings.NewReader(attributionNativeBody))
	request.Header.Set("X-Request-Id", id)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	return response
}

func (h *nativeAttributionHarness) rows(t *testing.T, id string) []model.Log {
	t.Helper()

	var rows []model.Log
	require.NoError(t, h.db.Where("request_id = ?", id).Order("id").Find(&rows).Error)

	return rows
}

func (h *nativeAttributionHarness) setTask(t *testing.T, id string, changes map[string]any) {
	t.Helper()
	require.NoError(t, h.db.Model(&model.NativeTask{}).Where("id = ?", id).Updates(changes).Error)
}

// Plan §1.4 and C9, one case per branch after the reservation: every row a
// request writes once money may be held carries the op and reads failed; the
// wallet's three refusals hold nothing and stay rejected; after the
// reserved->submitting transition the only answer is 202 and the only row is
// the task's own. Old: no op on any rejection row (rejected), and a later
// conflict answered 409 with a second row.
func TestNativeRowsAfterReserveCarryOperation(t *testing.T) {
	start := time.Date(2026, 10, 12, 1, 2, 3, 456_789_000, time.UTC)

	requireOneFailedRowWithOp := func(t *testing.T, h *nativeAttributionHarness, id string, status int, code string) {
		t.Helper()

		rows := h.rows(t, id)
		require.Len(t, rows, 1)
		require.Equal(t, status, rows[0].Code)
		require.Equal(t, code, rows[0].ErrorCode)
		require.Equal(t, model.EmptyNullString("native:"+id), rows[0].BillingOperationID)
		require.Equal(t, model.CallStatusFailed, rows[0].CallStatus(time.Now()))
		require.Equal(t, model.RequestSourcePlayground, rows[0].RequestSource)
	}

	for name, configure := range map[string]func(*nativeAttributionHarness){
		"admit ambiguous":     func(h *nativeAttributionHarness) { h.wallet.admitErr = errors.New("prepayment outcome unknown") },
		"begin_attempt error": func(h *nativeAttributionHarness) { h.wallet.beginErr = errors.New("prepayment unavailable") },
		"not claimed":         func(h *nativeAttributionHarness) { h.wallet.unclaimed = true },
	} {
		t.Run(name, func(t *testing.T) {
			h := newNativeAttributionHarness(t)
			h.start = start
			configure(h)

			response := h.submit(t, "case")
			require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			requireOneFailedRowWithOp(t, h, "case", http.StatusServiceUnavailable, "native_execution_unavailable")
		})
	}

	for name, refusal := range map[string]struct {
		err    error
		status int
	}{
		"insufficient balance": {balance.ErrPrepaymentInsufficientBalance, http.StatusPaymentRequired},
		"too many tasks":       {balance.ErrPrepaymentTooManyActiveTasks, http.StatusTooManyRequests},
		"model paused":         {balance.ErrPrepaymentModelPaused, http.StatusServiceUnavailable},
	} {
		t.Run("wallet refusal "+name, func(t *testing.T) {
			h := newNativeAttributionHarness(t)
			h.start = start
			h.wallet.admitErr = refusal.err

			response := h.submit(t, "refused")
			require.Equal(t, refusal.status, response.Code, response.Body.String())

			rows := h.rows(t, "refused")
			require.Len(t, rows, 1)
			require.Empty(t, rows[0].BillingOperationID)
			require.Equal(t, model.CallStatusRejected, rows[0].CallStatus(time.Now()))
		})
	}

	t.Run("success takes the hold's time", func(t *testing.T) {
		h := newNativeAttributionHarness(t)
		h.start = start

		response := h.submit(t, "ok")
		require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())

		rows := h.rows(t, "ok")
		require.Len(t, rows, 1)
		require.Equal(t, 202, rows[0].Code)
		require.Equal(t, model.UsageTime(start), rows[0].RequestAt.UTC(), "request_at = the receipt's usageAtMs")
		require.Equal(t, model.EmptyNullString("native:ok"), rows[0].BillingOperationID)

		admit := h.wallet.commands[0]
		require.Equal(t, "admit", admit.Action)
		require.NotNil(t, admit.APIKeyID)
		require.Equal(t, 1, *admit.APIKeyID)
		require.Equal(t, "brand/model/native", admit.UsageModelID)
		require.Equal(t, model.RequestSourcePlayground, admit.Source)
		require.Equal(t, start.UnixMilli(), *admit.UsageAtMs)
		require.Equal(t, rows[0].UsageIdentity(), model.UsageIdentity{
			APIKeyID: *admit.APIKeyID, ModelID: admit.UsageModelID, Source: admit.Source,
			UsageAt: time.UnixMilli(*admit.UsageAtMs).UTC(),
		})
	})

	t.Run("retry of a reserved task keeps the first admission time", func(t *testing.T) {
		h := newNativeAttributionHarness(t)
		h.start = start
		h.wallet.beginErr = errors.New("prepayment unavailable")

		first := h.submit(t, "retry")
		require.Equal(t, http.StatusServiceUnavailable, first.Code, first.Body.String())

		h.wallet.beginErr = nil
		h.start = start.Add(time.Minute)

		second := h.submit(t, "retry")
		require.Equal(t, http.StatusAccepted, second.Code, second.Body.String())

		rows := h.rows(t, "retry")
		require.Len(t, rows, 2)
		require.Equal(t, http.StatusServiceUnavailable, rows[0].Code)
		require.Equal(t, 202, rows[1].Code)
		require.Equal(t, model.UsageTime(start), rows[1].RequestAt.UTC(), "the first admission's time, not the retry's")
		require.Equal(t, rows[0].BillingOperationID, rows[1].BillingOperationID)
	})

	t.Run("lost reserved->submitting race answers 409 with the op", func(t *testing.T) {
		h := newNativeAttributionHarness(t)
		h.start = start
		h.wallet.onBegin = func() { h.setTask(t, "race", map[string]any{"status": "submitting"}) }

		response := h.submit(t, "race")
		require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
		requireOneFailedRowWithOp(t, h, "race", http.StatusConflict, "request_id_conflict")
	})

	for name, submit := range map[string]func(h *nativeAttributionHarness) func() (string, error){
		"conflict on the submission outcome": func(h *nativeAttributionHarness) func() (string, error) {
			return func() (string, error) {
				h.setTask(t, "late", map[string]any{"status": "queued", "upstream_id": "other-writer"})
				return "", errors.New("connection lost after write")
			}
		},
		"conflict on acceptance": func(h *nativeAttributionHarness) func() (string, error) {
			return func() (string, error) {
				h.setTask(t, "late", map[string]any{"status": "queued", "upstream_id": "other-writer"})
				return "upstream-1", nil
			}
		},
	} {
		t.Run(name+" answers the stored state", func(t *testing.T) {
			h := newNativeAttributionHarness(t)
			h.start = start
			h.provider.submit = submit(h)

			response := h.submit(t, "late")
			require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())

			rows := h.rows(t, "late")
			require.Len(t, rows, 1, "one request, one row: the task's own 202 row")
			require.Equal(t, 202, rows[0].Code)
			require.Equal(t, model.EmptyNullString("native:late"), rows[0].BillingOperationID)
		})
	}
}

// The admin native trial uses the request start like every other call, with
// source admin_demo. Old: the handler's time.Now().
func TestNativeTrialUsesRequestStart(t *testing.T) {
	gin.SetMode(gin.TestMode)

	database, err := model.OpenSQLite(filepath.Join(t.TempDir(), "trial.db"))
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.NativeTask{}, &model.Log{}))

	wallet := &nativeAttributionWallet{holdAt: map[string]int64{}, holdFrom: map[string]string{}}
	engine := &nativetask.Engine{DB: database, Wallet: wallet}
	channel := &model.Channel{ID: 9, Type: model.ChannelTypeFal, Status: model.ChannelStatusDisabled, Key: "trial-secret", BaseURL: "https://queue.fal.run"}
	deps := nativeTrialDependencies{
		engine: func() (*nativetask.Engine, bool) { return engine, true },
		token: func(id int) (*model.Token, error) {
			return &model.Token{ID: id, Name: "trial-key", GroupID: "internal", Status: model.TokenStatusEnabled}, nil
		},
		group: func(string, bool) (*model.Group, error) {
			return &model.Group{ID: "internal", Status: model.GroupStatusInternal}, nil
		},
		channel:  func(int) (*model.Channel, error) { return channel, nil },
		provider: func(*model.Channel) nativetask.Provider { return nativeAttributionProvider{} },
	}

	oldKey := config.AdminKey
	config.AdminKey = "private-admin-test"
	t.Cleanup(func() { config.AdminKey = oldKey })

	start := time.Now().Add(-3 * time.Second)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		middleware.SetRequestAt(c, start)
		c.Next()
	})
	router.POST("/api/native-trials/:group/:token", middleware.AdminAuth, deps.create)

	input := nativeTrialRequest{
		Contract: json.RawMessage(`{"version":1,"model":"test/model/native","input_schema":{"type":"object","required":["seed"],"properties":{"seed":{"type":"integer"}},"additionalProperties":false},"output_schema":{"type":"null"}}`),
		Input:    json.RawMessage(`{"seed":1}`), ChannelID: 9, Endpoint: "fal-ai/native/variant", CredentialScope: "scope",
		KeyFingerprint: model.ImageChannelKeyFingerprint(channel.Key), DeliveryBase: "https://gateway.example",
		Quote: json.RawMessage(`{"version":1,"currency":"USD","quoteVersion":"private-fixture","prepaidMicros":200,"routes":[{"routeId":"private","channelId":9,"provider":"fal","endpoint":"fal-ai/native/variant","credentialScope":"scope","quantityMetric":"request","estimatedMicros":100,"prepaidMicros":200,"rule":{"mode":"cost_markup","ratio":"1"}}]}`),
	}
	encoded, err := json.Marshal(input)
	require.NoError(t, err)

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/native-trials/internal/7", strings.NewReader(string(encoded)))
	request.Header.Set("Authorization", "Bearer "+config.AdminKey)
	request.Header.Set("X-Request-Id", "trial_start")

	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())

	require.Equal(t, "admit", wallet.commands[0].Action)
	require.Equal(t, model.RequestSourceAdminDemo, wallet.commands[0].Source)
	require.Equal(t, start.UnixMilli(), *wallet.commands[0].UsageAtMs)

	var rows []model.Log
	require.NoError(t, database.Where("request_id = ?", "trial_start").Find(&rows).Error)
	require.Len(t, rows, 1)
	require.Equal(t, model.UsageTime(start), rows[0].RequestAt.UTC())
	require.Equal(t, model.RequestSourceAdminDemo, rows[0].RequestSource)
	require.Equal(t, "trial-key", rows[0].TokenName)
}
