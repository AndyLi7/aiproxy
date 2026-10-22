//nolint:testpackage // Opt-in loopback harness uses the real controllers.
package controller

import (
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type agentLoopbackTransport struct{ real http.RoundTripper }

func (t agentLoopbackTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	req := r.Clone(r.Context())
	target := *r.URL
	req.URL = &target
	switch target.Hostname() {
	case "queue.fal.run", "agent-fixture.invalid":
		req.URL.Scheme = "http"
		req.URL.Host = "127.0.0.1:3512"
	case "127.0.0.1":
		if target.Port() != "3510" && target.Port() != "3511" && target.Port() != "3512" {
			return nil, errors.New("test outbound refused")
		}
	default:
		return nil, errors.New("test outbound refused")
	}
	return t.real.RoundTrip(req)
}

// Only outer provider networks are fake. Auth, native routing, wallet HTTP,
// archive storage and recovery execute their production implementations.
func TestAgentIntegrationHarness(t *testing.T) {
	if os.Getenv("AGENT_INTEGRATION_HARNESS") != "true" {
		t.Skip("opt-in isolated localhost harness")
	}
	root := os.Getenv("AGENT_INTEGRATION_STATE")
	require.True(t, filepath.IsAbs(root))
	require.Contains(t, root, "agent-integration")
	require.NoError(t, os.MkdirAll(root, 0700))
	database, err := model.OpenSQLite(filepath.Join(root, "gateway.db"))
	require.NoError(t, err)
	model.DB, model.LogDB = database, database
	common.UsingSQLite = true
	common.RedisEnabled = false
	config.AdminKey = "image-agent-local-fake-key"
	config.DisableModelConfig = false
	config.NativeRouteFollowsBinding = true
	require.NoError(t, database.AutoMigrate(&model.Group{}, &model.GroupModelConfig{}, &model.Token{}, &model.Channel{}, &model.ModelConfig{}, &model.NativeTask{}, &model.Log{}, &model.SummaryMinute{}, &model.GroupSummaryMinute{}))
	t.Setenv("EXTERNAL_BALANCE_URL", "http://127.0.0.1:3510")
	t.Setenv("EXTERNAL_BALANCE_KEY", "image-agent-local-fake-key")
	balance.Default = balance.NewExternalHTTP("http://127.0.0.1:3510", "image-agent-local-fake-key")
	http.DefaultTransport = agentLoopbackTransport{real: http.DefaultTransport}
	var count int64
	require.NoError(t, database.Model(&model.Channel{}).Count(&count).Error)
	if count == 0 {
		channel := model.Channel{ID: 11, Name: "agent-local-fixture", Type: model.ChannelTypeFal, Status: model.ChannelStatusEnabled, Key: "image-agent-local-fake-key", Models: []string{"local-fixture/image", "local-fixture/conversation"}}
		require.NoError(t, database.Create(&channel).Error)
		contract := map[string]any{"version": 1, "model": "local-fixture/image", "input_schema": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"prompt"}, "properties": map[string]any{"prompt": map[string]any{"type": "string", "minLength": 1, "maxLength": 6000}, "image_urls": map[string]any{"type": "array", "maxItems": 4, "items": map[string]any{"type": "string", "pattern": "^data:image/png;base64,[A-Za-z0-9+/=]+$"}}}}, "output_schema": map[string]any{"type": "object", "required": []string{"images"}, "properties": map[string]any{"images": map[string]any{"type": "array", "minItems": 1, "maxItems": 1, "items": map[string]any{"type": "object", "required": []string{"url"}, "properties": map[string]any{"url": map[string]any{"type": "string"}}}}}}, "artifacts": []any{map[string]any{"path": []string{"images", "*", "url"}}}}
		quote := map[string]any{"version": 1, "currency": "USD", "settlementPolicy": "actual-cost-v1", "quoteVersion": "agent-test-media-v1", "prepaidMicros": 30000, "routes": []any{map[string]any{"routeId": "agent-local-image", "channelId": 11, "provider": "fal", "endpoint": "fal-ai/agent-fixture", "credentialScope": "agent-test-fixture", "estimatedMicros": 20000, "prepaidMicros": 30000, "quantityMetric": "bounded_request", "rule": map[string]any{"mode": "cost_markup", "ratio": "1"}}}}
		native := map[string]any{"version": 1, "channelId": 11, "endpoint": "fal-ai/agent-fixture", "credentialScope": "agent-test-fixture", "keyFingerprint": model.ImageChannelKeyFingerprint(channel.Key), "deliveryBase": "https://agent-gateway.invalid", "contract": contract}
		image := model.ModelConfig{Model: "local-fixture/image", Type: mode.NativeTasks, Config: map[model.ModelConfigKey]any{NativeResultConfigKey: native, model.ImagePrepaymentConfigKey: quote, AgentExecutionConfigKey: map[string]any{"version": 1, "kind": "image", "upstreamModelId": "fal-ai/agent-fixture", "promptField": "prompt", "referenceField": "image_urls", "referenceFormat": "data-uri-array", "defaults": map[string]any{}}}}
		llm := model.ModelConfig{Model: "local-fixture/conversation", Type: mode.ChatCompletions, Price: model.Price{InputPrice: 1, InputPriceUnit: 1_000_000, OutputPrice: 2, OutputPriceUnit: 1_000_000}, Config: map[model.ModelConfigKey]any{AgentExecutionConfigKey: map[string]any{"version": 1, "kind": "inference", "upstreamModelId": "local-fixture/conversation", "maxInputTokens": 32000, "maxOutputTokens": 1200, "vision": true, "toolCalling": true, "imageTokensIncluded": true, "reasoningTokensIncluded": true}, "x_token_platform_pricing": map[string]any{"currency": "USD", "pricing_version": "agent-test-llm-v1"}}}
		require.NoError(t, database.Create(&image).Error)
		require.NoError(t, database.Create(&llm).Error)
	}
	require.NoError(t, model.InitModelConfigAndChannelCache())
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery(), middleware.RequestIDMiddleware, middleware.NewLog(logrus.StandardLogger()))
	api := router.Group("/api", middleware.AdminAuth)
	api.POST("/group/:group/ensure", EnsureGroup)
	api.GET("/group/:group", GetGroup)
	api.POST("/group/:group/status", UpdateGroupStatus)
	api.GET("/model_config/*model", GetModelConfig)
	api.POST("/model_config/*model", SaveModelConfig)
	api.POST("/token/:group", AddGroupToken)
	api.GET("/token/:group/search", SearchGroupTokens)
	api.GET("/token/:group/:id", GetGroupToken)
	api.POST("/token/:group/:id/status", UpdateGroupTokenStatus)
	api.POST("/token/:group/:id/name", UpdateGroupTokenName)
	v1 := router.Group("/v1", middleware.OperationalLogMiddleware(), middleware.TokenAuth)
	v1.GET("/agent-access", AgentAccess)
	v1.GET("/models", ListModels)
	v1.POST("/model-tasks", NativeTasks()...)
	v1.GET("/model-tasks/:id", GetNativeTask)
	v1.POST("/model-tasks/:id/cancel", CancelNativeTask)
	v1.GET("/model-tasks/:id/artifacts/:index", GetNativeTaskArtifact)
	api.GET("/_test/evidence", func(c *gin.Context) {
		var tasks []model.NativeTask
		database.Find(&tasks)
		safe := []map[string]any{}
		for _, task := range tasks {
			safe = append(safe, map[string]any{"id": task.ID, "group": task.GroupID, "tokenId": task.TokenID, "status": task.Status, "billingOperationId": task.BillingOperationID})
		}
		c.JSON(200, safe)
	})
	api.POST("/_test/reload", func(c *gin.Context) {
		if model.InitModelConfigAndChannelCache() != nil {
			c.Status(500)
		} else {
			c.Status(204)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				engine, ok := NativeTaskEngine()
				if ok {
					_, _ = engine.RecoverOnce(ctx, "agent-integration", time.Now(), nativetask.ResolveFalPoller(model.GetChannelByID), func(ctx context.Context, task string, index int, source string) (ownedartifact.Receipt, error) {
						return ownedartifact.StoreWithSourceClient(ctx, task, index, source, http.DefaultClient)
					})
				}
			}
		}
	}()
	t.Log("Agent gateway HTTP ready at 127.0.0.1:3511")
	require.NoError(t, http.ListenAndServe("127.0.0.1:3511", router))
}
