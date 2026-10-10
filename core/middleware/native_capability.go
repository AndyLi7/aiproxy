package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
)

// Native model tasks address a published capability by its public id; channel
// mappings and model configs are keyed by the internal capability route key.
func resolveNativeCapability(c *gin.Context, requestMode mode.Mode, publicID string) string {
	if requestMode != mode.NativeTasks || strings.Contains(publicID, "::") {
		return publicID
	}

	caches := GetModelCaches(c)
	if caches == nil {
		return publicID
	}

	return model.ResolveNativeCapabilityRoute(publicID, caches.EnabledModelConfigsMap)
}
