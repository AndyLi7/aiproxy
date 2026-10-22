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

	// Admin evidence spans all enabled configs, rather than a customer's
	// entitlement subset. Use the same native resolver (and identity parsing)
	// as NewDistribute(mode.NativeTasks); never guess between conflicting IDs.
	configs := make([]model.ModelConfig, 0, len(cache.EnabledModelConfigsMap))
	for _, config := range cache.EnabledModelConfigsMap {
		configs = append(configs, config)
	}

	resolution, match := model.NativeRouteResolution{}, model.NativeRouteNotFound
	if !strings.Contains(publicID, "::") {
		resolution, match = model.ResolveNativeCapabilityRoute(publicID, configs)
	}

	route, capability := publicID, ""

	mapped := match == model.NativeRouteFound
	if mapped {
		route = resolution.Route
		if resolution.HasIdentity {
			capability = resolution.Identity.Capability
		}
	} else if match != model.NativeRouteAmbiguous {
		route, capability = model.ResolveImageCapabilityRoute(
			publicID,
			func(key string) (map[model.ModelConfigKey]any, bool) {
				config, ok := cache.ModelConfig.GetModelConfig(key)
				return config.Config, ok
			},
		)
		if index := strings.LastIndex(publicID, "/"); capability == "" && index > 0 {
			parent, parsedCapability := publicID[:index], model.ModelCapability(publicID[index+1:])
			if parsedCapability.Valid() {
				candidate, _ := model.BuildModelCapabilityKey(parent, parsedCapability)
				if config, ok := cache.ModelConfig.GetModelConfig(
					candidate,
				); ok &&
					model.ValidateModelCapabilityConfig(
						config.Config,
						parent,
						parsedCapability,
					) == nil {
					route, capability = candidate, string(parsedCapability)
				}
			}
		}

		mapped = capability != ""
	}

	_, registered := cache.ModelConfig.GetModelConfig(route)
	if match == model.NativeRouteAmbiguous {
		registered = false
	}

	channels := map[int]bool{}
	for _, routes := range cache.EnabledModel2ChannelsBySet {
		for _, channel := range routes[route] {
			if match == model.NativeRouteAmbiguous {
				continue
			}

			channels[channel.ID] = true
		}
	}

	status := "unmapped"
	if mapped {
		status = "resolved"
	} else if match == model.NativeRouteAmbiguous {
		status = "ambiguous"
	}

	middleware.SuccessResponse(c, gin.H{
		"public_id": publicID, "routing_model": route, "capability": capability,
		"registered": registered, "mapped": mapped, "enabled_channels": len(channels),
		"resolution_status": status,
		"config_ok":         mapped && registered && len(channels) > 0,
		"check_scope":       "global_enabled_config",
		"generation_status": "not_attempted",
	})
}
