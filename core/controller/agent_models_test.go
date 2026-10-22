//nolint:testpackage // Exercises private controllers with the real model/cache store.
package controller

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentModelOperationsKeepPublicMediaAndAuthorizePrivateInference(t *testing.T) {
	db, e := model.OpenSQLite(filepath.Join(t.TempDir(), "agent.db"))
	require.NoError(t, e)
	pool, _ := db.DB()
	defer pool.Close()
	oldDB, oldLog, oldSQLite, oldDisable, oldKey := model.DB, model.LogDB, common.UsingSQLite, config.DisableModelConfig, config.AdminKey
	model.DB, model.LogDB = db, db
	common.UsingSQLite = true
	config.DisableModelConfig = false
	config.AdminKey = "agent-operations-test-only"
	t.Cleanup(func() {
		model.DB, model.LogDB = oldDB, oldLog
		common.UsingSQLite = oldSQLite
		config.DisableModelConfig = oldDisable
		config.AdminKey = oldKey
	})
	require.NoError(t, db.AutoMigrate(&model.ModelConfig{}, &model.Channel{}, &model.GroupModelConfig{}))
	channel := model.Channel{ID: 11, Type: model.ChannelTypeFal, Status: model.ChannelStatusEnabled, Models: []string{"public-image"}, Key: "test-only"}
	require.NoError(t, db.Create(&channel).Error)
	fixture := newNativeRouteFixture()
	fixture.quote["routes"].([]any)[0].(map[string]any)["quantityMetric"] = "bounded_request"
	contract := map[string]any{"version": 1, "model": "public-image", "input_schema": map[string]any{"type": "object", "required": []string{"prompt"}, "properties": map[string]any{"prompt": map[string]any{"type": "string"}, "image_urls": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "count": map[string]any{"type": "integer", "default": 1}}}, "output_schema": map[string]any{"type": "object", "properties": map[string]any{"image": map[string]any{"type": "string"}}}, "artifacts": []any{map[string]any{"path": []string{"image"}}}}
	fixture.binding["contract"] = contract
	mc := model.ModelConfig{Model: "public-image", Type: mode.NativeTasks, Config: map[model.ModelConfigKey]any{NativeResultConfigKey: fixture.binding, model.ImagePrepaymentConfigKey: fixture.quote, "unrelated": "preserved"}}
	require.NoError(t, db.Create(&mc).Error)
	require.NoError(t, model.InitModelConfigAndChannelCache())
	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := router.Group("/api", middleware.AdminAuth)
	api.GET("/agent-models", GetAgentModelOptions)
	api.POST("/agent-models", ConfigureAgentModel)
	request := func(body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), "POST", "/api/agent-models", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	input := `{"kind":"inference","id":"new","upstreamModelId":"vendor/chat","maxInputTokens":32000,"maxOutputTokens":1200,"vision":true,"inputUSD":1,"outputUSD":2,"sets":["default"]}`
	require.Equal(t, 401, request(input, "").Code)
	require.Equal(t, 400, request(strings.Replace(input, `"inputUSD":1`, `"inputUSD":-1`, 1), config.AdminKey).Code)
	require.Equal(t, 400, request(strings.Replace(input, `"id":"new"`, `"id":"new","key":"forbidden"`, 1), config.AdminKey).Code)
	require.Equal(t, 200, request(input, config.AdminKey).Code)
	require.Equal(t, 200, request(input, config.AdminKey).Code, "same create request is idempotent")
	require.Equal(t, 409, request(strings.Replace(input, `"inputUSD":1`, `"inputUSD":9`, 1), config.AdminKey).Code, "cannot overwrite an existing product")
	caches := model.LoadModelCaches()
	token := model.TokenCache{}
	token.SetAvailableSets([]string{"default"})
	token.SetModelsBySet(caches.EnabledModelsBySet)
	require.Equal(t, "agent-inference/new", token.FindModel("agent-inference/new"))
	token.SetAvailableSets([]string{"other"})
	require.Empty(t, token.FindModel("agent-inference/new"))
	token.SetAvailableSets([]string{"default"})
	listed := publicModelsForToken(token, caches.EnabledModelConfigsMap)
	require.Len(t, listed, 1)
	require.Equal(t, "public-image", listed[0].ID)
	stale, e := model.GetModelConfig("public-image")
	require.NoError(t, e)
	out := request(`{"kind":"image","model":"public-image"}`, config.AdminKey)
	require.Equal(t, 200, out.Code, out.Body.String())
	saved, e := model.GetModelConfig("public-image")
	require.NoError(t, e)
	require.False(t, model.IsAgentOnlyModel(saved))
	require.Equal(t, "preserved", saved.Config["unrelated"])
	oldNative, _ := json.Marshal(stale.Config[NativeResultConfigKey])
	newNative, _ := json.Marshal(saved.Config[NativeResultConfigKey])
	require.Equal(t, string(oldNative), string(newNative))
	oldQuote, _ := json.Marshal(stale.Config[model.ImagePrepaymentConfigKey])
	newQuote, _ := json.Marshal(saved.Config[model.ImagePrepaymentConfigKey])
	require.Equal(t, string(oldQuote), string(newQuote))
	require.Error(t, model.AttachAgentMapping(stale, map[string]any{"invalid": true}), "stale opt-in must not overwrite concurrent config")
	caches = model.LoadModelCaches()
	token.SetModelsBySet(caches.EnabledModelsBySet)
	listed = publicModelsForToken(token, caches.EnabledModelConfigsMap)
	require.Len(t, listed, 1)
	require.Equal(t, "public-image", listed[0].ID)
	delete(saved.Config, model.AgentVisibilityKey)
	require.True(t, model.IsAgentOnlyModel(saved), "legacy private descriptor stays private")
	contract["input_schema"].(map[string]any)["required"] = []string{"prompt", "unsupported"}
	fixture.binding["contract"] = contract
	// Required unknown properties cannot be supplied by this minimal tool.
	contract["input_schema"].(map[string]any)["properties"].(map[string]any)["unsupported"] = map[string]any{"type": "object"}
	mc.Config[NativeResultConfigKey] = fixture.binding
	_, ok := agentImageMapping(mc)
	require.False(t, ok)
}
