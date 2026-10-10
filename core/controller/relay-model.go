package controller

import (
	"encoding/json"
	"github.com/labring/aiproxy/core/common/registryvalidation"
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
	listed, _ := publicModelIndexForToken(token, enabledModelConfigsMap)
	return listed
}

// publicModelIndexForToken lists the models the key may call by the ID
// customers call each by, and indexes every other ID that still calls a
// listed entry (a capability ID behind a public_api_id, hidden aliases).
// Internal route keys ("::") are never listed.
func publicModelIndexForToken(
	token model.TokenCache,
	enabledModelConfigsMap map[string]model.ModelConfig,
) ([]*OpenAIModels, map[string]*OpenAIModels) {
	models := make(map[string]*OpenAIModels)
	accepted := make(map[string]*OpenAIModels)
	add := func(id string, mc model.ModelConfig) {
		if id == "" || strings.Contains(id, "::") {
			return
		}

		key := strings.ToLower(id)
		if _, exists := models[key]; exists {
			return
		}

		created := 0
		if !mc.CreatedAt.IsZero() {
			created = int(mc.CreatedAt.Unix())
		}
		models[key] = &OpenAIModels{
			ID:         id,
			Object:     "model",
			Created:    created,
			OwnedBy:    string(mc.Owner),
			Root:       id,
			Permission: permission,
			Parent:     nil,
		}
		encoded, _ := json.Marshal(mc.Config["x_token_platform_capability_contract"])
		var wrapper struct {
			Contract json.RawMessage `json:"contract"`
		}
		if json.Unmarshal(encoded, &wrapper) == nil {
			if discovery := registryvalidation.DiscoverImage(wrapper.Contract); discovery != nil {
				models[key].Generation = discovery.Generation
				models[key].API = discovery.API
				models[key].InputSchema = discovery.InputSchema
				models[key].SchemaURL = "/v1/models/" + id + "/schema"
			}
		}
	}

	var identities []model.PublicCapabilityIdentity
	token.Range(func(modelName string) bool {
		mc, ok := enabledModelConfigsMap[modelName]
		if !ok {
			return true
		}

		if identity, ok := model.PublicCapabilityIdentityFromConfig(mc); ok {
			identities = append(identities, identity)
		}

		add(model.ListedPublicModelID(mc), mc)

		return true
	})

	for _, identity := range identities {
		entry := models[strings.ToLower(identity.Callable())]
		if entry == nil {
			continue
		}

		for _, id := range identity.Accepted() {
			if _, claimed := accepted[id]; !claimed {
				accepted[id] = entry
			}
		}
	}

	result := make([]*OpenAIModels, 0, len(models))
	for _, entry := range models {
		result = append(result, entry)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})

	return result, accepted
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

	listed, accepted := publicModelIndexForToken(token, enabledModelConfigsMap)

	var found *OpenAIModels
	for _, candidate := range listed {
		if strings.EqualFold(candidate.ID, modelName) {
			found = candidate
			break
		}
	}

	// Any other ID that calls a listed model (exact) returns that entry.
	if found == nil {
		found = accepted[modelName]
	}

	if found == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": &relaymodel.OpenAIError{
				Message: "No model with this ID is available to this API key. Use an `id` from GET /v1/models.",
				Type:    "not_found_error",
				Param:   "model",
				Code:    "model_not_found",
			},
		})

		return
	}

	if c.GetBool("model_schema_request") {
		if found.InputSchema == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "schema_not_found", "type": "not_found_error", "message": "No public input schema is available for this model.", "param": "model"}})
			return
		}
		c.JSON(http.StatusOK, found.InputSchema)
		return
	}
	c.JSON(http.StatusOK, found)
}
