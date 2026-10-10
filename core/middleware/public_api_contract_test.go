package middleware

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicAuthenticationIncludesRequestIDAndSafeError(t *testing.T) {
	for _, path := range []string{"/v1/images/generations", "/v1/chat/completions", "/v1/audio/speech", "/v1/videos"} {
		r := gin.New()
		r.Use(RequestIDMiddleware)
		r.POST(path, TokenAuth)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(`{}`)))
		require.Equal(t, 401, w.Code)
		require.NotEmpty(t, w.Header().Get(RequestIDHeader))
		var body map[string]map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Equal(t, "authentication_error", body["error"]["type"])
		require.Equal(t, "invalid_api_key", body["error"]["code"])
		require.NotContains(t, w.Body.String(), "aiproxy")
	}
}
func TestInvalidModelDoesNotLookUpEmptyWallet(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"missing/model"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(Group, model.GroupCache{ID: "private-empty-group"})
	c.Set(Token, model.TokenCache{})
	c.Set(ModelCaches, &model.ModelCaches{EnabledModelConfigsMap: map[string]model.ModelConfig{}})
	distribute(c, mode.ImagesGenerations)
	require.Equal(t, 404, w.Code)
	require.Nil(t, GetGroupBalanceConsumerFromContext(c))
	require.NotContains(t, w.Body.String(), "private-empty-group")
}

func TestInvalidImageParametersDoNotLookUpEmptyWallet(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	const id = "vendor/image/text-to-image"
	c.Request = httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"vendor/image/text-to-image","prompt":""}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(Group, model.GroupCache{ID: "private-empty-group"})
	token := model.TokenCache{}
	token.SetAvailableSets([]string{"default"})
	token.SetModelsBySet(map[string][]string{"default": {id}})
	c.Set(Token, token)
	mc := model.ModelConfig{Model: id, Type: mode.ImagesGenerations, Config: map[model.ModelConfigKey]any{
		"x_token_platform_capability_contract": map[string]any{
			"entry_id": id,
			"contract": map[string]any{
				"entry_id": id, "validation_version": 1,
				"input_schema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"prompt": map[string]any{"type": "string", "minLength": 1}},
					"required":   []string{"prompt"}, "additionalProperties": false,
				},
			},
		},
	}}
	c.Set(ModelCaches, &model.ModelCaches{
		ModelConfig:            imageCapabilityTestCache{id: mc},
		EnabledModelConfigsMap: map[string]model.ModelConfig{id: mc},
	})
	distribute(c, mode.ImagesGenerations)
	require.Equal(t, 400, w.Code, w.Body.String())
	require.Nil(t, GetGroupBalanceConsumerFromContext(c))
	require.NotContains(t, w.Body.String(), "private-empty-group")
}
