//nolint:testpackage
package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// A wallet refusal happens before any provider call, so the same X-Request-Id
// and body must succeed after a top-up, exactly once, without a double charge.
func TestImageTaskPrepaymentRefusalKeepsRequestIDRetryable(t *testing.T) {
	toppedUp := false
	admits := []string{}
	wallet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var command balance.PrepaymentCommand
		require.NoError(t, json.NewDecoder(r.Body).Decode(&command))
		if command.Action == "admit" {
			admits = append(admits, command.BillingOperationID)
			if !toppedUp {
				w.WriteHeader(http.StatusPaymentRequired)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 1, "message": "Insufficient balance"})
				return
			}
		}
		receipt := map[string]any{"id": command.BillingOperationID, "status": "pending", "prepaidMicros": 2000000}
		if command.Action == "begin_attempt" {
			receipt["claimed"] = true
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": receipt}))
	}))
	defer wallet.Close()
	oldBalance := balance.Default
	balance.Default = balance.NewExternalHTTP(wallet.URL, "local-only")
	t.Cleanup(func() { balance.Default = oldBalance })

	providerCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		_, _ = w.Write([]byte(`{"request_id":"upstream"}`))
	}))
	defer upstream.Close()

	raw, err := os.ReadFile("../common/registryvalidation/testdata/provider.json")
	require.NoError(t, err)
	var contract map[string]any
	require.NoError(t, json.Unmarshal(raw, &contract))
	contract["execution"] = map[string]any{"mode": "async", "output": "image"}

	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "retry.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}, &model.RequestDetail{}))
	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = oldLogDB })

	ch := &model.Channel{
		ID: 7, Type: model.ChannelTypeFal, Key: "secret", BaseURL: upstream.URL,
		Models: []string{"image"}, ModelMapping: map[string]string{"image": "fal-ai/test"},
		Configs: map[string]any{providerBindingsConfig: map[string]any{"image": map[string]any{
			"provider": "small", "id": "small", "revision": "1",
			"contractHash": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}}},
	}
	var quote map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{"version":1,"quoteVersion":"test-1","currency":"USD","prepaidMicros":2000000,"routes":[{"routeId":"primary","channelId":7,"provider":"fal","endpoint":"fal-ai/test","credentialScope":"channel-7","estimatedMicros":1000000,"prepaidMicros":2000000,"rule":{"mode":"list_ratio","ratio":"1"}}]}`), &quote))

	submit := func() (*httptest.ResponseRecorder, *gin.Context) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/images/tasks",
			bytes.NewBufferString(`{"model":"image","prompt":"hi","n":1}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Header.Set("X-Request-ID", "topup-retry")
		c.Request.Header.Set(AIProxyChannelHeader, "7")
		c.Set(middleware.Group, model.GroupCache{ID: "g", Status: model.GroupStatusInternal})
		c.Set(middleware.Token, model.TokenCache{ID: 1})
		c.Set(middleware.RequestModel, "image")
		c.Set(middleware.RoutingModel, "image")
		c.Set(middleware.RequestedModel, "image")
		c.Set(middleware.ModelCaches, &model.ModelCaches{ChannelsByID: map[int]*model.Channel{7: ch}})
		c.Set(middleware.ModelConfig, model.ModelConfig{
			Price: model.Price{PerRequestPrice: 1},
			Config: map[model.ModelConfigKey]any{
				"x_token_platform_capability_contract": map[string]any{"entry_id": "image", "contract": contract},
				"x_token_platform_pricing":             map[string]any{"currency": "USD", "pricing_version": "v1"},
				model.ImagePrepaymentConfigKey:         quote,
			},
		})
		c.Set(middleware.GroupBalance, &middleware.GroupBalanceConsumer{Group: "g", CheckBalance: func(float64) bool {
			t.Fatal("prepaid image tasks must not use the cached balance check")
			return false
		}})
		submitImageTask(c)
		return w, c
	}
	count := func(table any) int64 {
		var n int64
		require.NoError(t, db.Model(table).Count(&n).Error)
		return n
	}

	w, c := submit()
	require.Equal(t, http.StatusPaymentRequired, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"insufficient_balance"`)
	require.Zero(t, providerCalls)
	require.Zero(t, count(&model.ImageTask{}))
	require.Zero(t, count(&model.AsyncUsageInfo{}))
	require.Zero(t, count(&model.Log{}))
	fields := middleware.OperationalFieldsFromContext(c)
	require.Equal(t, model.FailureStageBalance, fields.FailureStage)
	require.Equal(t, "insufficient_balance", fields.ErrorCode)

	toppedUp = true
	w, _ = submit()
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	require.Equal(t, 1, providerCalls)
	require.Len(t, admits, 2)
	require.NotEqual(t, admits[0], admits[1], "a refused admission must not be reused as a charge identity")

	// The accepted task is now idempotent: replaying it never resubmits or recharges.
	w, _ = submit()
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	require.Equal(t, 1, providerCalls)
	require.Len(t, admits, 2)
	require.EqualValues(t, 1, count(&model.ImageTask{}))
}
