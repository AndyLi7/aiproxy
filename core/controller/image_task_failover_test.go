package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

func TestImageFailoverDurableRoutingAndReplay(t *testing.T) {
	for _, tc := range []struct {
		name            string
		primaryStatus   int
		allFail, pinned bool
		retries         int64
		wantAttempts    int
		wantStatus      string
	}{
		{name: "connection failure then backup", retries: 2, wantAttempts: 2, wantStatus: "queued"},
		{name: "all connections fail only once", allFail: true, retries: 10, wantAttempts: 2, wantStatus: "failed"},
		{name: "HTTP 503 unknown", primaryStatus: 503, retries: 2, wantAttempts: 1, wantStatus: "submission_unknown"},
		{name: "HTTP 429 unknown", primaryStatus: 429, retries: 2, wantAttempts: 1, wantStatus: "submission_unknown"},
		{name: "accepted stays", primaryStatus: 200, retries: 2, wantAttempts: 1, wantStatus: "queued"},
		{name: "invalid request stays", primaryStatus: 422, retries: 2, wantAttempts: 1, wantStatus: "failed"},
		{name: "pinned stays", pinned: true, retries: 2, wantAttempts: 1, wantStatus: "failed"},
		{name: "disabled stays", retries: 0, wantAttempts: 1, wantStatus: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "attempt.db"))
			require.NoError(t, err)
			require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))
			old := model.LogDB
			model.LogDB = db
			t.Cleanup(func() { model.LogDB = old })
			raw, err := os.ReadFile("../testdata/fal-minimax-contract.json")
			require.NoError(t, err)
			var wrapper map[string]any
			require.NoError(t, json.Unmarshal(raw, &wrapper))
			contract, err := json.Marshal(wrapper["contract"])
			require.NoError(t, err)
			primaryCalls, backupCalls := 0, 0
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls++
				w.WriteHeader(tc.primaryStatus)
				_, _ = w.Write([]byte(`{"request_id":"primary-job"}`))
			}))
			defer primary.Close()
			if tc.primaryStatus == 0 {
				primary.Close()
			}
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backupCalls++
				_, _ = w.Write([]byte(`{"request_id":"backup-job"}`))
			}))
			defer backup.Close()
			if tc.allFail {
				backup.Close()
			}
			const public = "minimax/image-01/text-to-image"
			first := &model.Channel{ID: 71, Type: model.ChannelTypeFal, Status: model.ChannelStatusEnabled, BaseURL: primary.URL, Key: "first", Models: []string{public}, ModelMapping: map[string]string{public: "fal-ai/minimax/image-01"}}
			second := &model.Channel{ID: 72, Type: model.ChannelTypeFal, Status: model.ChannelStatusEnabled, BaseURL: backup.URL, Key: "second", Models: []string{public}, ModelMapping: map[string]string{public: "fal-ai/minimax/image-01"}}
			body := []byte(`{"model":"minimax/image-01/text-to-image","prompt":"test","n":1,"aspect_ratio":"1:1","prompt_optimizer":false}`)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/images/tasks", bytes.NewReader(body))
			c.Set(middleware.RequestModel, public)
			c.Set(middleware.RoutingModel, public)
			c.Set(middleware.RequestedModel, public)
			c.Set(middleware.Group, model.GroupCache{ID: "g"})
			c.Set(middleware.Token, model.TokenCache{ID: 1})
			price := model.Price{PerRequestPrice: 0.5}
			c.Set(middleware.ModelConfig, model.ModelConfig{RetryTimes: tc.retries, Price: price, Config: map[model.ModelConfigKey]any{"x_token_platform_capability_contract": wrapper}})
			initial := &initialChannel{channel: first, designatedChannel: tc.pinned, migratedChannels: []*model.Channel{first, second}}
			c.Set("image_initial_channel", initial)
			mt := NewMetaByContext(c, first, mode.ImagesGenerations)
			original := &model.ImageTask{ID: "logical-request", GroupID: "g", TokenID: 1, Model: public, RequestModel: public, ValidationContract: string(contract), Fingerprint: "same", ExpectedImages: 1, ChannelType: first.Type, UpstreamModel: mt.ActualModel}
			saved, created, err := model.ReserveImageTask(original, &model.AsyncUsageInfo{RequestID: original.ID, RequestAt: time.Now(), GroupID: "g", TokenID: 1, ChannelID: first.ID, BaseURL: first.BaseURL, Price: price, PricingVersion: "v1"})
			require.NoError(t, err)
			require.True(t, created)
			dispatchImageTaskWithFailover(c, saved, &fal.Adaptor{}, mt, []byte(`{"prompt":"test","num_images":1}`))
			task, err := model.GetImageTask(original.ID, "g", 1)
			require.NoError(t, err)
			require.Equal(t, tc.wantStatus, task.Status)
			require.Len(t, task.Attempts, tc.wantAttempts)
			require.False(t, task.Attempts[len(task.Attempts)-1].Retry)
			if tc.allFail {
				require.Equal(t, "no_candidates", task.Attempts[len(task.Attempts)-1].Decision)
			}
			var info model.AsyncUsageInfo
			require.NoError(t, db.First(&info).Error)
			require.Equal(t, price, info.Price)
			require.Equal(t, "v1", info.PricingVersion)
			var entry model.Log
			require.NoError(t, db.First(&entry).Error)
			require.Contains(t, entry.Metadata, "channel_failover_attempts")
			if tc.wantAttempts == 2 {
				require.Equal(t, second.ID, info.ChannelID)
				require.Equal(t, backup.URL, info.BaseURL)
				require.Equal(t, second.ID, entry.ChannelID)
				require.Equal(t, "none", task.Attempts[0].PotentialCost)
				require.Equal(t, failover.NotAccepted, task.Attempts[0].Failure.Acceptance)
			} else {
				require.Zero(t, backupCalls)
			}
			_, created, err = model.ReserveImageTask(&model.ImageTask{ID: original.ID, GroupID: "g", TokenID: 1, Model: public, Fingerprint: "same"}, &model.AsyncUsageInfo{})
			require.NoError(t, err)
			require.False(t, created)
			before := primaryCalls + backupCalls
			replay, _ := gin.CreateTestContext(httptest.NewRecorder())
			replay.Request = c.Request
			dispatchImageTaskWithFailover(replay, task, &fal.Adaptor{}, mt, nil)
			require.Equal(t, before, primaryCalls+backupCalls)
			for _, table := range []any{&model.Log{}, &model.AsyncUsageInfo{}} {
				var count int64
				require.NoError(t, db.Model(table).Count(&count).Error)
				require.EqualValues(t, 1, count)
			}
		})
	}
}

// Real Seedream provider bindings must cross adapters, not just fal accounts.
func TestSeedreamFalToArkCandidate(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "cross-provider.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))
	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })
	raw, err := os.ReadFile("../common/registryvalidation/testdata/seedream45-task.json")
	require.NoError(t, err)
	var contract map[string]any
	require.NoError(t, json.Unmarshal(raw, &contract))
	spec := contract["providers"].(map[string]any)["volcengine"].(map[string]any)["upstream"].(map[string]any)
	const route = "bytedance/seedream-4.5::text-to-image-v2"
	ch := &model.Channel{ID: 72, Type: model.ChannelTypeDoubao, Status: model.ChannelStatusEnabled, Models: []string{route}, ModelMapping: map[string]string{route: "doubao-seedream-4-5-251128"}, Configs: map[string]any{providerBindingsConfig: map[string]any{route: map[string]any{"provider": "volcengine", "id": spec["id"], "revision": spec["revision"], "contractHash": spec["contractHash"]}}}}
	zero := int64(0)
	usage := &model.AsyncUsageInfo{RequestID: "cross-provider", UsageContext: model.UsageContext{ImageUsage: &model.ImageUsage{Version: 1, State: "incomplete", Scenario: "generation", InputCount: &zero, Outputs: []model.ImageUsageOutput{}}}}
	require.NoError(t, db.Create(usage).Error)
	for _, size := range []string{"auto_2K", "portrait_4_3"} {
		t.Run(size, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/images/tasks", bytes.NewBufferString(`{"model":"bytedance/seedream-4.5/text-to-image-v2","prompt":"test","n":1,"max_images":1,"image_size":"`+size+`"}`))
			c.Set(middleware.Group, model.GroupCache{ID: "g"})
			c.Set(middleware.Token, model.TokenCache{ID: 1})
			c.Set(middleware.RoutingModel, route)
			c.Set(middleware.RequestModel, route)
			c.Set(middleware.ModelConfig, model.ModelConfig{Price: model.Price{PerRequestPrice: 0.04}, Config: map[model.ModelConfigKey]any{"x_token_platform_capability_contract": map[string]any{"contract": contract}}})
			routing := &retryState{meta: NewMetaByContext(c, ch, mode.ImagesGenerations), migratedChannels: []*model.Channel{ch}, ignoreChannelIDs: map[int64]struct{}{}, failedChannelIDs: map[int64]struct{}{}}
			task := &model.ImageTask{UsageID: usage.ID, ExpectedImages: 1}
			selected, _, _, _ := nextImageTaskCandidate(c, t.Context(), task, routing)
			if size == "auto_2K" {
				require.NotNil(t, selected)
			} else {
				require.Nil(t, selected)
			}
		})
	}
}

func TestImageFailoverMeteringPreservesBillingBoundaries(t *testing.T) {
	zero, one := int64(0), int64(1)
	next := registryvalidation.ImageMeteringEvidence{MaximumOutputs: 1}
	require.True(t, compatibleImageFailoverMetering(next, 1, &model.ImageUsage{InputCount: &zero}, false))
	require.False(t, compatibleImageFailoverMetering(next, 1, &model.ImageUsage{InputCount: &zero}, true))
	require.False(t, compatibleImageFailoverMetering(next, 2, &model.ImageUsage{InputCount: &zero}, false))
	require.False(t, compatibleImageFailoverMetering(next, 1, &model.ImageUsage{InputCount: &one}, false))
	require.False(t, compatibleImageFailoverMetering(next, 1, &model.ImageUsage{}, false))
	next.InputCount = 1
	require.False(t, compatibleImageFailoverMetering(next, 1, nil, false))
}
