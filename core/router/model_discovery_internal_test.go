package router

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
)

func TestSlashfulModelDiscoveryAndSchemaUseEntitlements(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const id = "vendor/image/text-to-image"
	token := model.TokenCache{Models: []string{id}}
	token.SetAvailableSets([]string{model.ChannelDefaultSet})
	token.SetModelsBySet(map[string][]string{model.ChannelDefaultSet: {id}})
	var contract map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{"validation_version":1,"execution":{"mode":"async","output":"image"},"input_schema":{"type":"object","properties":{"prompt":{"type":"string"},"n":{"type":"integer","const":1}}}}`), &contract))
	caches := &model.ModelCaches{EnabledModelConfigsMap: map[string]model.ModelConfig{id: {Model: id, Config: map[model.ModelConfigKey]any{"x_token_platform_capability_contract": map[string]any{"contract": contract}}}}}
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set(middleware.Token, token); c.Set(middleware.ModelCaches, caches) })
	engine.GET("/v1/models/:model/*path", modelDetailOrOperation()...)
	for _, tc := range []struct {
		path     string
		status   int
		contains string
	}{
		{id, 200, `"max_images":1`}, {id + "/schema", 200, `"const":1`}, {"other/image/text-to-image", 404, `model_not_found`},
	} {
		r := httptest.NewRecorder()
		engine.ServeHTTP(r, httptest.NewRequest("GET", "/v1/models/"+tc.path, nil))
		require.Equal(t, tc.status, r.Code, r.Body.String())
		require.Contains(t, r.Body.String(), tc.contains)
	}
}

func TestModelDiscoveryKeepsGeminiOperationDispatch(t *testing.T) {
	engine := gin.New()
	handlers := modelDetailOrOperation()
	called := false
	engine.GET("/v1/models/:model/*path", handlers[0], func(c *gin.Context) {
		called = true
		require.Equal(t, "veo", c.Param("model"))
		require.Equal(t, "/op-123", c.Param("operation_id"))
		c.Status(204)
	})
	r := httptest.NewRecorder()
	engine.ServeHTTP(r, httptest.NewRequest("GET", "/v1/models/veo/operations/op-123", nil))
	require.True(t, called)
	require.Equal(t, 204, r.Code)
}
