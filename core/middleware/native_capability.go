package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
)

// entitledModelConfigs are the enabled configs the API key may call.
func entitledModelConfigs(c *gin.Context, token model.TokenCache) []model.ModelConfig {
	value, _ := c.Get(ModelCaches)

	caches, _ := value.(*model.ModelCaches)
	if caches == nil {
		return nil
	}

	entitled := make([]model.ModelConfig, 0)
	token.Range(func(modelName string) bool {
		if config, ok := caches.EnabledModelConfigsMap[modelName]; ok {
			entitled = append(entitled, config)
		}

		return true
	})

	return entitled
}

// nativeCapabilityRoute is a native request resolved to one capability config.
type nativeCapabilityRoute struct {
	route string
	// publicModel is the config's public_model: the model the request-log row
	// and the capability route-key check name (never the requested spelling).
	publicModel string
}

// resolveNativeCapability resolves a native task's model ID among the native
// configs the key may call (exact capability ID, public_api_id or alias). It
// returns nil when nothing matched, and false after answering a configuration
// conflict. Internal route keys ("::") are not resolved here.
func resolveNativeCapability(
	c *gin.Context,
	requestMode mode.Mode,
	token model.TokenCache,
	requested string,
) (*nativeCapabilityRoute, bool) {
	if requestMode != mode.NativeTasks || strings.Contains(requested, "::") {
		return nil, true
	}

	resolution, match := model.ResolveNativeCapabilityRoute(requested, entitledModelConfigs(c, token))
	switch match {
	case model.NativeRouteAmbiguous:
		setRequestedModelForLog(c, requested)
		abortNativeModelError(c, http.StatusServiceUnavailable, "model_route_unavailable", model.FailureStageRouting, nil)

		return nil, false
	case model.NativeRouteFound:
	default:
		return nil, true
	}

	route := &nativeCapabilityRoute{route: resolution.Route}
	c.Set(RequestedModel, requested)

	if resolution.HasIdentity {
		route.publicModel = resolution.Identity.PublicModel
		c.Set(PublicModel, resolution.Identity.PublicModel)
		c.Set(PublicCapabilityModel, resolution.Identity.CapabilityModel)
		c.Set(ResolvedCapability, resolution.Identity.Capability)
	}

	return route, true
}

// NativeModelSuggestions applies the native suggestion rules to requested
// among the configs the request's key may call.
func NativeModelSuggestions(c *gin.Context, requested string) model.NativeModelSuggestion {
	return model.SuggestNativeModels(requested, entitledModelConfigs(c, GetToken(c)))
}

// abortUnknownNativeModel answers a native task whose model ID calls nothing
// with the native envelope: 400 native_model_unavailable when the key may call
// that model on another endpoint, else 404 model_not_found, each with
// suggested_models. The requested ID is never echoed.
func abortUnknownNativeModel(c *gin.Context, token model.TokenCache, requested string) {
	suggestion := model.SuggestNativeModels(requested, entitledModelConfigs(c, token))

	status, code := http.StatusNotFound, "model_not_found"
	if suggestion.OtherEndpoint {
		status, code = http.StatusBadRequest, "native_model_unavailable"
	}

	setRequestedModelForLog(c, requested)
	abortNativeModelError(c, status, code, model.FailureStageModel, append([]string{}, suggestion.Models...))
}

// logModelColumnLength is the size of the request log's model column.
const logModelColumnLength = 128

// setRequestedModelForLog names the requested ID on a rejected request's log
// row: the requested_model metadata (up to 191 characters) and the model
// column (up to its 128).
func setRequestedModelForLog(c *gin.Context, requested string) {
	c.Set(RequestedModel, truncateOperationalText(requested, model.MaxPublicModelIDLength))
	c.Set(RequestModel, truncateOperationalText(requested, logModelColumnLength))
}

// abortNativeModelError writes the native envelope; a non-nil suggested adds
// param "model" and suggested_models.
func abortNativeModelError(
	c *gin.Context,
	status int,
	code string,
	stage model.FailureStage,
	suggested []string,
) {
	SetOperationalFailure(c, stage, code, nativetask.ErrorMessage(code))

	if suggested == nil {
		nativetask.WriteError(c.Writer, status, code)
	} else {
		nativetask.WriteErrorDetail(c.Writer, status, code, "model", suggested)
	}

	c.Abort()
}
