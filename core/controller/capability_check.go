package controller

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
)

// CheckCapabilityResolution uses the live routing cache, without a generation,
// token creation, balance mutation, or upstream request. Admin authentication is
// supplied by apiRouter. This is configuration evidence, not a customer dry run.
func CheckCapabilityResolution(c *gin.Context) {
	publicID := c.Query("model")
	if publicID == "" || len(publicID) > 256 || strings.ContainsAny(publicID, "\r\n") {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid model")
		return
	}
	cache := model.LoadModelCaches()
	if cache == nil || cache.ModelConfig == nil {
		middleware.ErrorResponse(c, http.StatusServiceUnavailable, "routing cache unavailable")
		return
	}
	route, capability := model.ResolveImageCapabilityRoute(publicID, func(key string) (map[model.ModelConfigKey]any, bool) {
		config, ok := cache.ModelConfig.GetModelConfig(key)
		return config.Config, ok
	})
	if index := strings.LastIndex(publicID, "/"); capability == "" && index > 0 {
		parent, cap := publicID[:index], model.ModelCapability(publicID[index+1:])
		if cap.Valid() {
			candidate, _ := model.BuildModelCapabilityKey(parent, cap)
			if config, ok := cache.ModelConfig.GetModelConfig(candidate); ok && model.ValidateModelCapabilityConfig(config.Config, parent, cap) == nil {
				route, capability = candidate, string(cap)
			}
		}
	}
	_, registered := cache.ModelConfig.GetModelConfig(route)
	channels := map[int]bool{}
	for _, routes := range cache.EnabledModel2ChannelsBySet {
		for _, channel := range routes[route] {
			channels[channel.ID] = true
		}
	}
	middleware.SuccessResponse(c, gin.H{
		"public_id": publicID, "routing_model": route, "capability": capability,
		"registered": registered, "mapped": capability != "", "enabled_channels": len(channels),
	})
}
