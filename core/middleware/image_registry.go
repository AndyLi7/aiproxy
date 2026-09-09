package middleware

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
)

func validateImageRegistryRequest(c *gin.Context, requestMode mode.Mode, publicID string, config map[model.ModelConfigKey]any) *registryvalidation.ValidationError {
	if requestMode != mode.ImagesGenerations && requestMode != mode.ImagesEdits {
		return nil
	}
	metadata, present := config["x_token_platform_capability_contract"]
	// Legacy image routes are not opted in. Platform capability routes fail closed.
	if !present && config["public_capability_model"] == nil {
		return nil
	}
	unavailable := &registryvalidation.ValidationError{Status: 503}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return unavailable
	}
	var wrapper struct {
		ID       string          `json:"entry_id"`
		Contract json.RawMessage `json:"contract"`
	}
	if json.Unmarshal(encoded, &wrapper) != nil || wrapper.ID == "" {
		return unavailable
	}
	if wrapper.ID != publicID {
		// Private demo projections bind a transport ID to the original registry
		// contract. Only server-owned configuration may establish this binding.
		parent, _ := config["public_model"].(string)
		capability, _ := config["capability"].(string)
		if parent == "" || (capability != "edit" && capability != "text-to-image") ||
			config["public_capability_model"] != parent+"/"+capability ||
			(publicID != parent+"::"+capability && publicID != parent+"/"+capability) {
			return unavailable
		}
	}
	if !common.IsJSONContentType(c.Request.Header.Get("Content-Type")) {
		return &registryvalidation.ValidationError{Status: 400}
	}
	body, err := common.GetRequestBodyReusable(c.Request)
	if err != nil {
		return &registryvalidation.ValidationError{Status: 400}
	}
	var input map[string]any
	if json.Unmarshal(body, &input) != nil || input == nil || input["model"] != publicID {
		return &registryvalidation.ValidationError{Status: 400}
	}
	input["model"] = wrapper.ID
	validationBody, err := json.Marshal(input)
	if err != nil {
		return &registryvalidation.ValidationError{Status: 400}
	}
	normalized, validationErr := registryvalidation.ValidateImage(wrapper.Contract, wrapper.ID, validationBody)
	if validationErr != nil {
		return validationErr
	}
	if json.Unmarshal(normalized, &input) != nil {
		return unavailable
	}
	input["model"] = publicID
	normalized, err = json.Marshal(input)
	if err != nil {
		return unavailable
	}
	common.SetRequestBody(c.Request, normalized)
	clearRequestBodyNode(c)
	return nil
}
