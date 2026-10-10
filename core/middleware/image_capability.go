package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
)

func resolveImageCapability(c *gin.Context, requestMode mode.Mode, publicID string) string {
	if requestMode != mode.ImagesGenerations && requestMode != mode.ImagesEdits {
		return publicID
	}

	route, capability := model.ResolveImageCapabilityRoute(
		publicID,
		func(key string) (map[model.ModelConfigKey]any, bool) {
			config, ok := GetModelCaches(c).ModelConfig.GetModelConfig(key)
			if !ok {
				return nil, false
			}

			return config.Config, true
		},
	)
	if capability != "" {
		c.Set(VideoCapability, capability) // Shared request-log capability field.
	}

	return route
}

// imageGroupModelMessage tells the caller a model group ID is not callable on
// the image endpoints and what to send instead.
const imageGroupModelMessage = "This model ID names a group of capabilities and cannot be called. " +
	"Do not resend it: use an ID from suggested_models or an `id` from GET /v1/models."

// abortUnknownImageModel answers an image generation request (POST
// /v1/images/generations or /v1/images/tasks) whose model ID calls nothing
// with 404 model_not_found, type not_found_error, param model and
// suggested_models: the capability IDs of the requested model group this key
// may call on this endpoint, or none. With DISABLE_IMAGE_GROUP_IDS set a model
// group ID gets this answer by design (owner decision D3) and its own message;
// otherwise it reaches here only when no capability of the group matched.
func abortUnknownImageModel(c *gin.Context, token model.TokenCache, requested string) {
	servable := make([]model.ModelConfig, 0)
	for _, candidate := range entitledModelConfigs(c, token) {
		if CheckRelayMode(mode.ImagesGenerations, candidate.Type) {
			servable = append(servable, candidate)
		}
	}

	suggested := model.CapabilityGroupSuggestions(requested, servable)

	message := nativetask.ErrorMessage("model_not_found")
	if len(suggested) > 0 && config.DisableImageGroupIDs {
		message = imageGroupModelMessage
	}

	setRequestedModelForLog(c, requested)
	SetOperationalFailure(c, model.FailureStageModel, "model_not_found", message)
	c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": relaymodel.OpenAIError{
		Code:            "model_not_found",
		Message:         message,
		Type:            "not_found_error",
		Param:           "model",
		SuggestedModels: &suggested,
	}})
}
