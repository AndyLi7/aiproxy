//nolint:testpackage
package task

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/consume"
	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/controller"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

// Requires tests/fixtures/sec-wallet-server.ts from the companion application.
// Actual wallet HTTP handlers + ledger; provider responses and admission context
// are local fixtures. No production credentials or paid provider calls.
func TestSECWalletIntegration(t *testing.T) {
	walletURL := os.Getenv("SEC_TEST_WALLET_URL")
	if walletURL == "" {
		t.Skip("start companion local wallet fixture and set SEC_TEST_WALLET_URL")
	}
	require.True(t, strings.HasPrefix(walletURL, "http://127.0.0.1:"), "local fixture only")
	const group = "u_user_1"
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "gateway.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.AsyncUsageInfo{}, &model.ConsumeError{}, &model.Log{}, &model.RequestDetail{}, &model.ImageTask{}, &model.Channel{}))
	oldDB, oldLog, oldBalance := model.DB, model.LogDB, balance.Default
	model.DB, model.LogDB = db, db
	balance.Default = balance.NewExternalHTTP(walletURL, "local-sec-test-only")
	t.Cleanup(func() { model.DB, model.LogDB, balance.Default = oldDB, oldLog, oldBalance })
	require.NoError(t, model.CacheSetGroup(&model.GroupCache{ID: group, Status: model.GroupStatusEnabled}))
	t.Cleanup(func() { require.NoError(t, model.CacheDeleteGroup(group)) })
	mc := model.ModelConfig{Config: map[model.ModelConfigKey]any{"x_token_platform_pricing": map[string]any{"currency": "USD", "pricing_version": "sec-test"}}}
	ledgerCount := func(trace string, want int, micros int64) {
		req, e := http.NewRequest(http.MethodGet, walletURL+"/_test/ledger", nil)
		require.NoError(t, e)
		req.Header.Set("Authorization", "Bearer local-sec-test-only")
		resp, e := http.DefaultClient.Do(req)
		require.NoError(t, e)
		defer resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)
		var entries []struct {
			Trace  string `json:"source_request_id"`
			Amount int64  `json:"amount_micros"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&entries))
		var count int
		var total int64
		for _, row := range entries {
			if row.Trace == trace {
				count++
				total += row.Amount
			}
		}
		require.Equal(t, want, count, trace)
		require.Equal(t, micros, total, trace)
	}
	contextFor := func(trace string) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		c.Request.Header.Set("X-Request-ID", trace)
		middleware.RequestIDMiddleware(c)
		c.Set(middleware.Group, model.GroupCache{ID: group, Status: model.GroupStatusEnabled})
		c.Set(middleware.Token, model.TokenCache{ID: 1, Name: "sec-test"})
		c.Set(middleware.ModelConfig, mc)
		c.Set(middleware.RequestModel, "sec-model")
		return c
	}
	t.Run("sync_same_client_trace_two_debits", func(t *testing.T) {
		for range 2 {
			c := contextFor("sec-sync-repeat")
			m := controller.NewMetaByContext(c, &model.Channel{ID: 1}, mode.ChatCompletions)
			_, consumer, e := balance.Default.GetGroupRemainBalance(t.Context(), m.Group)
			require.NoError(t, e)
			consume.Consume(t.Context(), time.Now(), consumer, time.Now(), 200, m, model.Usage{}, model.UsageContext{}, model.Price{PerRequestPrice: 0.1}, "", "", 0, nil, true, nil, "", model.AsyncUsageStatusNone)
		}
		ledgerCount("sec-sync-repeat", 2, -200000)
	})
	t.Run("video_persist_reload_same_trace_two_debits", func(t *testing.T) {
		for range 2 {
			c := contextFor("sec-video-repeat")
			m := controller.NewMetaByContext(c, &model.Channel{ID: 1}, mode.Videos)
			entry := &model.Log{RequestID: model.EmptyNullString(m.RequestID), AsyncUsageStatus: model.AsyncUsageStatusPending}
			require.NoError(t, db.Create(entry).Error)
			info := &model.AsyncUsageInfo{RequestID: m.RequestID, BillingOperationID: m.BillingOperationID, GroupID: group, TokenID: 1, TokenName: "sec-test", PricingCurrency: "USD", PricingVersion: "sec-test", Price: model.Price{PerRequestPrice: 0.2}, LogID: entry.ID}
			require.NoError(t, model.CreateAsyncUsageInfo(info))
			var fresh model.AsyncUsageInfo
			require.NoError(t, db.First(&fresh, info.ID).Error)
			require.NoError(t, db.Model(&fresh).Update("next_poll_at", time.Now().Add(-time.Minute)).Error)
			fresh.NextPollAt = time.Now().Add(-time.Minute)
			claimed, e := claimAsyncUsage(&fresh)
			require.NoError(t, e)
			require.True(t, claimed)
			require.NoError(t, completeAsyncUsage(t.Context(), &fresh, model.Usage{}, model.UsageContext{}))
		}
		ledgerCount("sec-video-repeat", 2, -400000)
	})
	t.Run("lost_wallet_reply_retries_one_debit", func(t *testing.T) {
		ctx := context.WithValue(t.Context(), balance.CtxRequestID, "sec-lost-reply")
		ctx = balance.ContextWithPricing(balance.ContextWithBillingOperationID(ctx, "sec-operation-lost"), "USD", "sec-test")
		_, consumer, e := balance.Default.GetGroupRemainBalance(ctx, model.GroupCache{ID: group})
		require.NoError(t, e)
		_, e = consumer.PostGroupConsume(ctx, "sec-test", 0.3)
		require.NoError(t, e)
		_, e = consumer.PostGroupConsume(ctx, "sec-test", 0.3)
		require.NoError(t, e)
		ledgerCount("sec-lost-reply", 1, -300000)
	})
	t.Run("preupgrade_null_operation_settles_and_retries", func(t *testing.T) {
		for _, alreadyCharged := range []bool{false, true} {
			trace := fmt.Sprintf("sec-legacy-%t", alreadyCharged)
			ctx := balance.ContextWithPricing(context.WithValue(t.Context(), balance.CtxRequestID, trace), "USD", "sec-test")
			if alreadyCharged {
				_, consumer, e := balance.Default.GetGroupRemainBalance(ctx, model.GroupCache{ID: group})
				require.NoError(t, e)
				_, e = consumer.PostGroupConsume(ctx, "sec-test", 0.4)
				require.NoError(t, e)
			}
			entry := &model.Log{RequestID: model.EmptyNullString(trace), AsyncUsageStatus: model.AsyncUsageStatusPending}
			require.NoError(t, db.Create(entry).Error)
			info := &model.AsyncUsageInfo{RequestID: trace, GroupID: group, TokenName: "sec-test", PricingCurrency: "USD", PricingVersion: "sec-test", Price: model.Price{PerRequestPrice: 0.4}, LogID: entry.ID}
			require.NoError(t, model.CreateAsyncUsageInfo(info))
			require.NoError(t, db.Model(info).Update("billing_operation_id", nil).Error)
			var fresh model.AsyncUsageInfo
			require.NoError(t, db.First(&fresh, info.ID).Error)
			require.Empty(t, fresh.BillingOperationID)
			require.NoError(t, db.Model(&fresh).Update("next_poll_at", time.Now().Add(-time.Minute)).Error)
			fresh.NextPollAt = time.Now().Add(-time.Minute)
			claimed, e := claimAsyncUsage(&fresh)
			require.NoError(t, e)
			require.True(t, claimed)
			require.NoError(t, completeAsyncUsage(ctx, &fresh, model.Usage{}, model.UsageContext{}))
			ledgerCount(trace, 1, -400000)
		}
	})
	t.Run("old_gateway_payload_new_wallet", func(t *testing.T) {
		body := `{"group":"u_user_1","tokenName":"sec-test","amount":0.5,"requestId":"sec-old-gateway","currency":"USD","pricingVersion":"sec-test"}`
		for range 2 {
			req, e := http.NewRequest(http.MethodPost, walletURL+"/api/internal/wallet/consume", strings.NewReader(body))
			require.NoError(t, e)
			req.Header.Set("Authorization", "Bearer local-sec-test-only")
			req.Header.Set("Content-Type", "application/json")
			resp, e := http.DefaultClient.Do(req)
			require.NoError(t, e)
			var result struct{ Code int }
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
			resp.Body.Close()
			require.Zero(t, result.Code)
		}
		ledgerCount("sec-old-gateway", 1, -500000)
	})
	t.Run("image_same_id_same_content_one_generation_one_debit", func(t *testing.T) {
		fixture, e := os.ReadFile("../testdata/fal-minimax-contract.json")
		require.NoError(t, e)
		var metadata map[string]any
		require.NoError(t, json.Unmarshal(fixture, &metadata))
		submissions := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost {
				submissions++
				fmt.Fprint(w, `{"request_id":"sec-upstream"}`)
			} else if strings.HasSuffix(r.URL.Path, "/status") {
				fmt.Fprint(w, `{"status":"COMPLETED"}`)
			} else {
				fmt.Fprint(w, `{"images":[{"url":"https://cdn.example/local-test.png"}]}`)
			}
		}))
		defer upstream.Close()
		const public = "minimax/image-01/text-to-image"
		ch := &model.Channel{Type: model.ChannelTypeFal, Key: "local-test-key", BaseURL: upstream.URL, Models: []string{public}, ModelMapping: map[string]string{public: "fal-ai/minimax/image-01"}}
		require.NoError(t, db.Create(ch).Error)
		contract, e := json.Marshal(metadata["contract"])
		require.NoError(t, e)
		handlers := controller.ImageTasks()
		router := gin.New()
		router.POST("/v1/images/tasks", func(c *gin.Context) {
			middleware.RequestIDMiddleware(c)
			raw, readErr := io.ReadAll(c.Request.Body)
			require.NoError(t, readErr)
			normalized, validationErr := registryvalidation.ValidateImage(contract, public, raw)
			require.Nil(t, validationErr)
			common.SetRequestBody(c.Request, normalized)
			c.Set(middleware.Group, model.GroupCache{ID: group, Status: model.GroupStatusEnabled})
			c.Set(middleware.Token, model.TokenCache{ID: 1, Name: "sec-test"})
			c.Set(middleware.ChannelID, ch.ID)
			c.Set(middleware.RequestModel, "minimax/image-01")
			c.Set(middleware.RoutingModel, public)
			c.Set(middleware.RequestedModel, public)
			c.Set(middleware.ModelCaches, &model.ModelCaches{ChannelsByID: map[int]*model.Channel{ch.ID: ch}, EnabledModel2ChannelsBySet: map[string]map[string][]*model.Channel{model.ChannelDefaultSet: {public: {ch}}}})
			c.Set(middleware.ModelConfig, model.ModelConfig{Price: model.Price{ImageOutputPrice: 0.25, ImageOutputPriceUnit: 1}, Config: map[model.ModelConfigKey]any{"x_token_platform_capability_contract": metadata, "x_token_platform_pricing": map[string]any{"currency": "USD", "pricing_version": "sec-test"}}})
			c.Set(middleware.GroupBalance, &middleware.GroupBalanceConsumer{Group: group, CheckBalance: func(float64) bool { return true }})
		}, handlers[0], handlers[2])
		for range 2 {
			req := httptest.NewRequest(http.MethodPost, "/v1/images/tasks", bytes.NewBufferString(`{"model":"minimax/image-01/text-to-image","prompt":"test","n":1}`))
			req.Header.Set("X-Request-ID", "sec-image-same")
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, 202, w.Code, w.Body.String())
		}
		require.Equal(t, 1, submissions)
		var info model.AsyncUsageInfo
		require.NoError(t, db.Where("request_id = ?", "sec-image-same").First(&info).Error)
		require.NoError(t, db.Model(&info).Update("next_poll_at", time.Now().Add(-time.Minute)).Error)
		info.NextPollAt = time.Now().Add(-time.Minute)
		claimed, e := claimAsyncUsage(&info)
		require.NoError(t, e)
		require.True(t, claimed)
		processOneImageUsage(t.Context(), &info)
		var final model.AsyncUsageInfo
		require.NoError(t, db.First(&final, info.ID).Error)
		require.Equal(t, model.AsyncUsageStatusCompleted, final.Status)
		require.True(t, final.BalanceConsumed)
		claimed, e = claimAsyncUsage(&final)
		require.NoError(t, e)
		require.False(t, claimed)
		ledgerCount("sec-image-same", 1, -250000)
		require.Equal(t, 1, submissions)
	})
}
