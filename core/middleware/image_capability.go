package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
)

func resolveImageCapability(c *gin.Context, requestMode mode.Mode, publicID string) string {
	if requestMode != mode.ImagesGenerations && requestMode != mode.ImagesEdits {
		return publicID
	}
	route, capability := model.ResolveImageCapabilityRoute(publicID, func(key string) (map[model.ModelConfigKey]any, bool) {
		config, ok := GetModelCaches(c).ModelConfig.GetModelConfig(key)
		if !ok {
			return nil, false
		}
		return config.Config, true
	})
	if capability != "" {
		c.Set(VideoCapability, capability) // Shared request-log capability field.
	}
	return route
}
