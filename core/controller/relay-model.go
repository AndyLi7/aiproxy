package controller

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
)

func publicModelsForToken(
	token model.TokenCache,
	enabledModelConfigsMap map[string]model.ModelConfig,
) []*OpenAIModels {
	models := make(map[string]*OpenAIModels)
	add := func(id string, owner model.ModelOwner) {
		if id == "" {
			return
		}
		key := strings.ToLower(id)
		if _, exists := models[key]; exists {
			return
		}
		models[key] = &OpenAIModels{
			ID:         id,
			Object:     "model",
			Created:    1626777600,
			OwnedBy:    string(owner),
			Root:       id,
			Permission: permission,
			Parent:     nil,
		}
	}

	token.Range(func(modelName string) bool {
		mc, ok := enabledModelConfigsMap[modelName]
		if !ok {
			return true
		}
		if metadata, capability := model.CapabilityRoutingMetadataFromConfig(mc); capability {
			add(metadata.PublicModel, mc.Owner)
			add(metadata.PublicCapabilityModel, mc.Owner)
			return true
		}
		add(modelName, mc.Owner)
		return true
	})

	result := make([]*OpenAIModels, 0, len(models))
	for _, entry := range models {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result
}

// ListModels godoc
//
//	@Summary		List models
//	@Description	List all models
//	@Tags			relay
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Success		200	{object}	object{object=string,data=[]OpenAIModels}
//	@Router			/v1/models [get]
func ListModels(c *gin.Context) {
	enabledModelConfigsMap := middleware.GetModelCaches(c).EnabledModelConfigsMap
	token := middleware.GetToken(c)

	availableOpenAIModels := publicModelsForToken(token, enabledModelConfigsMap)

	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   availableOpenAIModels,
	})
}

// RetrieveModel godoc
//
//	@Summary		Retrieve model
//	@Description	Retrieve a model
//	@Tags			relay
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Success		200	{object}	OpenAIModels
//	@Router			/v1/models/{model} [get]
func RetrieveModel(c *gin.Context) {
	token := middleware.GetToken(c)
	modelName := c.Param("model")
	enabledModelConfigsMap := middleware.GetModelCaches(c).EnabledModelConfigsMap
	var found *OpenAIModels
	for _, candidate := range publicModelsForToken(token, enabledModelConfigsMap) {
		if strings.EqualFold(candidate.ID, modelName) {
			found = candidate
			break
		}
	}
	if found == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": &relaymodel.OpenAIError{
				Message: fmt.Sprintf("the model '%s' does not exist", modelName),
				Type:    "invalid_request_error",
				Param:   "model",
				Code:    "model_not_found",
			},
		})

		return
	}

	c.JSON(http.StatusOK, found)
}
