package middleware

import (
	"encoding/json"
	"regexp"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	gatewayconfig "github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
)

var imageCapabilitySlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func validateImageRegistryRequest(
	c *gin.Context,
	requestMode mode.Mode,
	publicID string,
	config map[model.ModelConfigKey]any,
) *registryvalidation.ValidationError {
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
	// Resolve base-model aliases only from distributor-owned context and the
	// selected server configuration, and only while the image endpoints accept
	// model group IDs (DISABLE_IMAGE_GROUP_IDS unset; owner decision D3).
	// Preserve the original body model below.
	bindingID := publicID
	if !gatewayconfig.DisableImageGroupIDs &&
		GetRequestedModel(c) == publicID && GetPublicModel(c) == publicID &&
		GetPublicCapabilityModel(c) != "" &&
		config["public_capability_model"] == GetPublicCapabilityModel(c) &&
		config["capability"] == GetResolvedCapability(c) {
		bindingID = GetPublicCapabilityModel(c)
	}

	if wrapper.ID != bindingID {
		// Private demo projections bind a transport ID to the original registry
		// contract. Only server-owned configuration may establish this binding.
		parent, _ := config["public_model"].(string)

		capability, _ := config["capability"].(string)
		if parent == "" || (len(capability) > 80 || !imageCapabilitySlug.MatchString(capability)) ||
			config["public_capability_model"] != parent+"/"+capability ||
			(bindingID != parent+"::"+capability && bindingID != parent+"/"+capability) {
			return unavailable
		}
	}

	if c.Request.URL.Path == "/v1/images/generations" {
		var execution struct {
			Execution struct {
				Mode string `json:"mode"`
			} `json:"execution"`
		}
		if json.Unmarshal(wrapper.Contract, &execution) != nil {
			return unavailable
		}
		if execution.Execution.Mode == "async" {
			return &registryvalidation.ValidationError{Status: 400, Param: "endpoint", Code: "unsupported_endpoint", Message: "This model uses asynchronous image generation. Submit to /v1/images/tasks and poll /v1/images/tasks/{id}.", Expected: map[string]any{"endpoint": "/v1/images/tasks", "status_endpoint": "/v1/images/tasks/{id}"}}
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

	normalized, validationErr := registryvalidation.ValidateImage(
		wrapper.Contract,
		wrapper.ID,
		validationBody,
	)
	if validationErr != nil {
		return validationErr
	}

	// Decode into a fresh map: Unmarshal merges into existing maps and would
	// resurrect consumed aliases (size/image) from the original request.
	input = nil
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
