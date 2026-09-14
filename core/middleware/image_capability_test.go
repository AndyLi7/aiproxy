package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

type imageCapabilityTestCache map[string]model.ModelConfig

func (cache imageCapabilityTestCache) GetModelConfig(key string) (model.ModelConfig, bool) {
	config, ok := cache[key]
	return config, ok
}

func TestImageCapabilityMappingPreservesAuthorization(t *testing.T) {
	publicID, route := "vendor/image/text-to-image", "vendor/image::text-to-image"
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(ModelCaches, &model.ModelCaches{ModelConfig: imageCapabilityTestCache{route: {Config: map[model.ModelConfigKey]any{
		"capability_contract_version": 1, "public_model": "vendor/image", "public_capability_model": publicID, "capability": "text-to-image",
	}}}})
	require.Equal(t, route, resolveImageCapability(c, mode.ImagesGenerations, publicID))
	require.Equal(t, "text-to-image", GetVideoCapability(c))
	token := &model.TokenCache{}
	token.SetAvailableSets([]string{"customer"})
	token.SetModelsBySet(map[string][]string{"other": {route}})
	require.Empty(t, token.FindModel(route), "mapping must not grant access to another set")
	token.SetModelsBySet(map[string][]string{"customer": {route}})
	require.Equal(t, route, token.FindModel(route))
	require.Equal(t, publicID, resolveImageCapability(c, mode.ChatCompletions, publicID))
	require.Equal(t, publicID, resolveImageCapability(c, mode.Videos, publicID))
}
