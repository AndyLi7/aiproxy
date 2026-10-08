//nolint:testpackage
package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/imageprepayment"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// prepaidWallet is a D34 wallet stub installed as balance.Default. It records
// every action (settle with its outcome), claims a new attempt only once every
// earlier attempt was released, as the real wallet does, and answers HTTP 500
// to reject_attempt while rejectDown is set.
type prepaidWallet struct {
	mu         sync.Mutex
	actions    []string
	open       map[string]bool
	settled    bool
	rejectDown bool
}

func newPrepaidWallet(t *testing.T) *prepaidWallet {
	t.Helper()
	wallet := &prepaidWallet{open: map[string]bool{}}
	server := httptest.NewServer(wallet)
	t.Cleanup(server.Close)
	old := balance.Default
	balance.Default = balance.NewExternalHTTP(server.URL, "local-only")
	t.Cleanup(func() { balance.Default = old })
	return wallet
}

func (p *prepaidWallet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var command balance.PrepaymentCommand
	if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
		http.Error(w, "bad command", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	action := command.Action
	if command.Outcome != nil {
		action += ":" + command.Outcome.Kind + "/" + command.Outcome.Reason
	}
	p.actions = append(p.actions, action)
	receipt := map[string]any{"id": command.BillingOperationID, "status": "pending", "prepaidMicros": 2000000}
	switch command.Action {
	case "begin_attempt":
		receipt["claimed"] = len(p.open) == 0
		if len(p.open) == 0 {
			p.open[command.AttemptID] = true
		}
	case "reject_attempt":
		if p.rejectDown {
			http.Error(w, "wallet unavailable", http.StatusInternalServerError)
			return
		}
		delete(p.open, command.AttemptID)
	case "settle":
		p.settled = true
	}
	if p.settled {
		receipt["status"], receipt["chargedMicros"], receipt["refundMicros"] = "refunded", 0, 2000000
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": receipt})
}

func (p *prepaidWallet) Actions() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.actions...)
}

func (p *prepaidWallet) SetRejectDown(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rejectDown = down
}

func useImageTaskTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "image-task.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}, &model.RequestDetail{}))
	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })
	return db
}

// submitPinnedPrepaidImageTask runs POST /v1/images/tasks for a prepaid fal
// model pinned to channel 7, whose provider is served at upstreamURL.
func submitPinnedPrepaidImageTask(t *testing.T, requestID, upstreamURL string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := os.ReadFile("../common/registryvalidation/testdata/provider.json")
	require.NoError(t, err)
	var contract map[string]any
	require.NoError(t, json.Unmarshal(raw, &contract))
	contract["execution"] = map[string]any{"mode": "async", "output": "image"}

	ch := &model.Channel{
		ID: 7, Type: model.ChannelTypeFal, Key: "fal-image-secret-key", BaseURL: upstreamURL,
		Models: []string{"image"}, ModelMapping: map[string]string{"image": "fal-ai/test"},
		Configs: map[string]any{providerBindingsConfig: map[string]any{"image": map[string]any{
			"provider": "small", "id": "small", "revision": "1",
			"contractHash": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}}},
	}
	var quote map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{"version":1,"quoteVersion":"test-1","currency":"USD","prepaidMicros":2000000,"routes":[{"routeId":"primary","channelId":7,"provider":"fal","endpoint":"fal-ai/test","credentialScope":"channel-7","estimatedMicros":1000000,"prepaidMicros":2000000,"rule":{"mode":"list_ratio","ratio":"1"}}]}`), &quote))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/images/tasks",
		bytes.NewBufferString(`{"model":"image","prompt":"a private customer prompt","n":1}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("X-Request-ID", requestID)
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
	submitImageTask(c)
	return w
}

// exhaustedFalAccount answers every submission like fal does for an account
// with no balance left, and counts the calls.
func exhaustedFalAccount(t *testing.T, calls *int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*calls++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"detail":"User is locked. Reason: Exhausted balance. Top up your balance at fal.ai/dashboard/billing."}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// The 2026-10-08 incident on the image lane: fal answers an exhausted account
// with 403. The task fails at once with upstream_unavailable, the hold is
// released and refunded in full in the same request, the request log carries
// the code, and operators get one log line with the status and fal's reason
// (never the key or the prompt).
func TestImageTaskProviderUnavailableFailsAndRefundsAtOnce(t *testing.T) {
	hook := new(logtest.Hook)
	oldHooks := logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks))
	logrus.AddHook(hook)
	t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(oldHooks) })

	wallet := newPrepaidWallet(t)
	providerCalls := 0
	upstream := exhaustedFalAccount(t, &providerCalls)
	db := useImageTaskTestDB(t)

	w := submitPinnedPrepaidImageTask(t, "locked-account", upstream.URL)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	require.Equal(t, 1, providerCalls)

	var response model.ImageTask
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Equal(t, "failed", response.Status)
	require.Equal(t, &model.ImageTaskError{Code: "upstream_unavailable", Message: model.UpstreamUnavailableMessage}, response.Error)
	require.NotNil(t, response.Billing)
	require.Equal(t, "refunded", response.Billing.Status)
	// No failover follows (the channel is pinned), so the failure is stored
	// first and one Sync releases the attempt and settles the hold.
	require.Equal(t, []string{"admit", "begin_attempt", "reject_attempt", "settle:failed/platform_failure", "get"}, wallet.Actions())

	saved, err := model.GetImageTask("locked-account", "g", 1)
	require.NoError(t, err)
	require.True(t, saved.BillingSettled)
	var entry model.Log
	require.NoError(t, db.Where("request_id = ?", "locked-account").First(&entry).Error)
	require.Equal(t, "upstream_unavailable", entry.ErrorCode)
	require.Equal(t, model.AsyncUsageStatusFailed, entry.AsyncUsageStatus)

	// Customers see the fixed message, not the masked generation_failed.
	customer, _ := gin.CreateTestContext(httptest.NewRecorder())
	customer.Set(middleware.Group, model.GroupCache{ID: "g", Status: model.GroupStatusEnabled})
	require.Equal(t, &model.ImageTaskError{Code: "upstream_unavailable", Message: model.UpstreamUnavailableMessage}, publicImageTask(customer, saved).Error)

	var logged *logrus.Entry
	for _, e := range hook.AllEntries() {
		require.NotContains(t, e.Message+fmt.Sprint(e.Data), "fal-image-secret-key")
		require.NotContains(t, e.Message+fmt.Sprint(e.Data), "a private customer prompt")
		if e.Message == "image task provider submission failed" {
			logged = e
		}
	}
	require.NotNil(t, logged)
	require.Equal(t, "image", logged.Data["lane"])
	require.Equal(t, "locked-account", logged.Data["task_id"])
	require.Equal(t, "fal-ai/test", logged.Data["endpoint"])
	require.Equal(t, 403, logged.Data["provider_status"])
	require.Contains(t, logged.Data["provider_reason"], "Exhausted balance")
	require.Equal(t, "not_accepted", logged.Data["acceptance"])
}

// A wallet outage while releasing the refused attempt must not leave the task
// "submitting": it would surface 15 minutes later as generation_timeout. The
// task fails at once with upstream_unavailable, and the next Sync (prepayment
// recovery) releases the attempt and refunds the hold.
func TestImageTaskProviderUnavailableFailsWhenWalletReleaseFails(t *testing.T) {
	wallet := newPrepaidWallet(t)
	wallet.SetRejectDown(true)
	providerCalls := 0
	upstream := exhaustedFalAccount(t, &providerCalls)
	db := useImageTaskTestDB(t)

	w := submitPinnedPrepaidImageTask(t, "wallet-blip", upstream.URL)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	require.Equal(t, 1, providerCalls)

	var response model.ImageTask
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Equal(t, "failed", response.Status)
	require.Equal(t, &model.ImageTaskError{Code: "upstream_unavailable", Message: model.UpstreamUnavailableMessage}, response.Error)
	require.Equal(t, []string{"admit", "begin_attempt", "reject_attempt"}, wallet.Actions())

	saved, err := model.GetImageTask("wallet-blip", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "failed", saved.Status)
	require.Equal(t, "upstream_unavailable", saved.Error.Code)
	require.False(t, saved.BillingSettled)
	var entry model.Log
	require.NoError(t, db.Where("request_id = ?", "wallet-blip").First(&entry).Error)
	require.Equal(t, "upstream_unavailable", entry.ErrorCode)
	require.Equal(t, model.AsyncUsageStatusFailed, entry.AsyncUsageStatus)

	wallet.SetRejectDown(false)
	receipt, err := imageprepayment.Sync(t.Context(), saved)
	require.NoError(t, err)
	require.Equal(t, "refunded", receipt.Status)
	require.Equal(t, []string{"admit", "begin_attempt", "reject_attempt", "reject_attempt", "settle:failed/platform_failure", "get"}, wallet.Actions())
	settled, err := model.GetImageTask("wallet-blip", "g", 1)
	require.NoError(t, err)
	require.True(t, settled.BillingSettled)
	require.Equal(t, "upstream_unavailable", settled.Error.Code)
}

// On a prepaid failover the refused attempt is released before the next
// channel is claimed. If the wallet cannot release it, no other channel can be
// claimed: the task fails with upstream_unavailable (not "submitting") and is
// settled by the next Sync.
func TestImageFailoverPrepaidReleaseBeforeSwitch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rejectDown bool
	}{
		{name: "released then backup"},
		{name: "release fails so no switch", rejectDown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wallet := newPrepaidWallet(t)
			wallet.SetRejectDown(tc.rejectDown)
			db := useImageTaskTestDB(t)
			primaryCalls, backupCalls := 0, 0
			primary := exhaustedFalAccount(t, &primaryCalls)
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				backupCalls++
				_, _ = w.Write([]byte(`{"request_id":"backup-job"}`))
			}))
			defer backup.Close()

			raw, err := os.ReadFile("../testdata/fal-minimax-contract.json")
			require.NoError(t, err)
			var wrapper map[string]any
			require.NoError(t, json.Unmarshal(raw, &wrapper))
			contract, err := json.Marshal(wrapper["contract"])
			require.NoError(t, err)
			const public = "minimax/image-01/text-to-image"
			const endpoint = "fal-ai/minimax/image-01"
			first := &model.Channel{ID: 71, Type: model.ChannelTypeFal, Status: model.ChannelStatusEnabled, BaseURL: primary.URL, Key: "first", Models: []string{public}, ModelMapping: map[string]string{public: endpoint}}
			second := &model.Channel{ID: 72, Type: model.ChannelTypeFal, Status: model.ChannelStatusEnabled, BaseURL: backup.URL, Key: "second", Models: []string{public}, ModelMapping: map[string]string{public: endpoint}}
			quote := `{"version":1,"quoteVersion":"test-1","currency":"USD","prepaidMicros":2000000,"routes":[` +
				`{"routeId":"primary","channelId":71,"provider":"fal","endpoint":"` + endpoint + `","credentialScope":"channel-71","estimatedMicros":1000000,"prepaidMicros":2000000,"rule":{"mode":"list_ratio","ratio":"1"}},` +
				`{"routeId":"backup","channelId":72,"provider":"fal","endpoint":"` + endpoint + `","credentialScope":"channel-72","estimatedMicros":1000000,"prepaidMicros":2000000,"rule":{"mode":"list_ratio","ratio":"1"}}]}`
			_, err = model.ParseImagePrepaymentQuote(quote)
			require.NoError(t, err)

			body := []byte(`{"model":"minimax/image-01/text-to-image","prompt":"test","n":1,"aspect_ratio":"1:1","prompt_optimizer":false}`)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/images/tasks", bytes.NewReader(body))
			c.Set(middleware.RequestModel, public)
			c.Set(middleware.RoutingModel, public)
			c.Set(middleware.RequestedModel, public)
			c.Set(middleware.Group, model.GroupCache{ID: "g"})
			c.Set(middleware.Token, model.TokenCache{ID: 1})
			price := model.Price{PerRequestPrice: 0.5}
			c.Set(middleware.ModelConfig, model.ModelConfig{RetryTimes: 2, Price: price, Config: map[model.ModelConfigKey]any{"x_token_platform_capability_contract": wrapper}})
			c.Set("image_initial_channel", &initialChannel{channel: first, migratedChannels: []*model.Channel{first, second}})
			mt := NewMetaByContext(c, first, mode.ImagesGenerations)
			original := &model.ImageTask{
				ID: "prepaid-failover", GroupID: "g", TokenID: 1, Model: public, RequestModel: public,
				ValidationContract: string(contract), Fingerprint: "same", ExpectedImages: 1,
				ChannelType: first.Type, UpstreamModel: mt.ActualModel,
				PrepaymentQuoteJSON: quote, BillingOperationID: "op-prepaid-failover",
			}
			saved, created, err := model.ReserveImageTask(original, &model.AsyncUsageInfo{RequestID: original.ID, RequestAt: time.Now(), GroupID: "g", TokenID: 1, ChannelID: first.ID, BaseURL: first.BaseURL, Price: price, PricingVersion: "v1"})
			require.NoError(t, err)
			require.True(t, created)

			dispatchImageTaskWithFailover(c, saved, &fal.Adaptor{}, mt, []byte(`{"prompt":"test","num_images":1}`))
			require.Equal(t, 1, primaryCalls)
			task, err := model.GetImageTask(original.ID, "g", 1)
			require.NoError(t, err)

			if !tc.rejectDown {
				require.Equal(t, 1, backupCalls)
				require.Equal(t, "queued", task.Status)
				require.Len(t, task.Attempts, 2)
				require.Equal(t, []string{"begin_attempt", "reject_attempt", "begin_attempt", "accept_attempt", "get"}, wallet.Actions())
				return
			}

			require.Zero(t, backupCalls)
			require.Equal(t, "failed", task.Status)
			require.Equal(t, &model.ImageTaskError{Code: "upstream_unavailable", Message: model.UpstreamUnavailableMessage}, task.Error)
			require.Len(t, task.Attempts, 1)
			require.False(t, task.Attempts[0].Retry)
			require.Equal(t, "no_candidates", task.Attempts[0].Decision)
			require.Equal(t, first.ID, task.Attempts[0].ChannelID)
			require.False(t, task.BillingSettled)
			// The release before the switch failed; the Sync after the failure
			// was stored retried it once.
			require.Equal(t, []string{"begin_attempt", "reject_attempt", "reject_attempt"}, wallet.Actions())
			var entry model.Log
			require.NoError(t, db.Where("request_id = ?", original.ID).First(&entry).Error)
			require.Equal(t, "upstream_unavailable", entry.ErrorCode)
			require.Equal(t, first.ID, entry.ChannelID)

			wallet.SetRejectDown(false)
			receipt, err := imageprepayment.Sync(t.Context(), task)
			require.NoError(t, err)
			require.Equal(t, "refunded", receipt.Status)
			require.Equal(t, []string{"begin_attempt", "reject_attempt", "reject_attempt", "reject_attempt", "settle:failed/platform_failure", "get"}, wallet.Actions())
			settled, err := model.GetImageTask(original.ID, "g", 1)
			require.NoError(t, err)
			require.True(t, settled.BillingSettled)
		})
	}
}
