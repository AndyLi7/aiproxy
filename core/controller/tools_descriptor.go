package controller

// RECONSTRUCTED. Native contracts remain authoritative; API retail pricing is
// deliberately not a prerequisite for an internal execution binding.
import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
)

func toolsDescriptor(
	mc model.ModelConfig,
) (map[string]any, nativeRouteBinding, *model.Channel, bool) {
	binding, ok := parseNativeRouteBinding(mc)
	if !ok {
		return nil, binding, nil, false
	}

	channel, err := model.GetChannelByID(binding.ChannelID)
	if err != nil || channel == nil || channel.Status != model.ChannelStatusEnabled ||
		channel.Type != model.ChannelTypeFal ||
		channel.Key == "" ||
		model.ImageChannelKeyFingerprint(channel.Key) != binding.KeyFingerprint ||
		(channel.BaseURL != "" && strings.TrimRight(channel.BaseURL, "/") != "https://queue.fal.run") {
		return nil, binding, nil, false
	}

	origin, e := url.Parse(binding.DeliveryBase)
	if e != nil || origin.Scheme != "https" || origin.Hostname() == "" || origin.User != nil ||
		origin.RawQuery != "" ||
		origin.Fragment != "" ||
		strings.Trim(origin.Path, "/") != "" ||
		binding.CredentialScope == "" ||
		!agentUpstream.MatchString(binding.Endpoint) {
		return nil, binding, nil, false
	}

	if _, err = nativeresult.CompileTaskContract(binding.Contract); err != nil {
		return nil, binding, nil, false
	}

	var contract nativeresult.TaskContract
	if json.Unmarshal(binding.Contract, &contract) != nil || contract.Model != mc.Model {
		return nil, binding, nil, false
	}

	capability, _ := mc.Config[model.ModelConfigCapabilityKey].(string)
	mapping, valid := toolsInputMapping(contract, binding.Contract, capability, binding.Endpoint)

	return mapping, binding, channel, valid
}

func toolsInputMapping(
	contract nativeresult.TaskContract,
	contractRaw json.RawMessage,
	capability, endpoint string,
) (map[string]any, bool) {
	var schema struct {
		Type       string                    `json:"type"`
		Properties map[string]map[string]any `json:"properties"`
	}
	if json.Unmarshal(contract.InputSchema, &schema) != nil || schema.Type != "object" {
		return nil, false
	}

	var kind string
	switch capability {
	case "text-to-image", "image-to-image", "edit":
		kind = "image"
	case "text-to-video", "image-to-video":
		kind = "video"
	case "text-to-speech":
		kind = "audio"
	default:
		return nil, false
	}

	promptField := "prompt"
	if kind == "audio" {
		promptField = "text"
	}

	if schema.Properties[promptField]["type"] != "string" {
		return nil, false
	}

	var reference any

	format := "none"
	if schema.Properties["image_urls"] != nil {
		strict := agentDataURIReferences(contract.InputSchema) &&
			(len(contract.UpstreamInputSchema) == 0 || agentDataURIReferences(contract.UpstreamInputSchema))
		if !strict && !toolsFalDataURIReferences(contractRaw) {
			return nil, false
		}

		reference = "image_urls"
		format = "data-uri-array"
	}

	if (capability == "image-to-image" || capability == "edit" || capability == "image-to-video") &&
		reference == nil {
		return nil, false
	}

	parameters, defaults, ok := toolsInputParameters(
		schema.Properties,
		contract.FixedParameters,
		promptField,
	)
	if !ok {
		return nil, false
	}

	mapping := map[string]any{
		"version":         1,
		"kind":            kind,
		"capability":      capability,
		"upstreamModelId": endpoint,
		"promptField":     promptField,
		"referenceField":  reference,
		"referenceFormat": format,
		"parameters":      parameters,
		"defaults":        defaults,
	}

	return mapping, true
}

func toolsInputParameters(
	properties map[string]map[string]any,
	fixedParameters map[string]json.RawMessage,
	promptField string,
) (map[string]string, map[string]any, bool) {
	defaults := map[string]any{}
	parameters := map[string]string{}

	semantic := map[string]string{
		"image_size":      "size",
		"seed":            "seed",
		"num_images":      "count",
		"negative_prompt": "negativePrompt",
		"aspect_ratio":    "aspectRatio",
		"width":           "width",
		"height":          "height",
		"duration":        "duration",
	}
	for name, spec := range properties {
		if !agentField.MatchString(name) {
			return nil, nil, false
		}

		if name == promptField || name == "image_urls" {
			continue
		}

		if _, fixed := fixedParameters[name]; fixed {
			continue
		}

		if key, exists := semantic[name]; exists {
			parameters[key] = name
		}

		switch v := spec["default"].(type) {
		case string:
			if len(v) <= 1000 {
				defaults[name] = v
			}
		case bool, float64:
			defaults[name] = v
		}
	}

	if properties["num_images"] == nil && properties["max_images"] != nil {
		parameters["count"] = "max_images"
	}

	return parameters, defaults, true
}

func toolsRevision(mapping map[string]any, binding nativeRouteBinding) (string, error) {
	b, err := json.Marshal([]any{mapping, binding})
	if err != nil {
		return "", err
	}

	return toolsDigest(b), nil
}

func GetToolsModelOptions(c *gin.Context) {
	var rows []model.ToolsModelBinding
	if model.DB.Find(&rows).Error != nil {
		toolsError(c, 503, "model_route_unavailable")
		return
	}

	items := []map[string]any{}
	seen := map[string]bool{}

	add := func(mc model.ModelConfig) {
		if m, _, _, ok := toolsDescriptor(mc); ok {
			items = append(
				items,
				map[string]any{
					"model":      mc.Model,
					"kind":       m["kind"],
					"capability": m["capability"],
					"vision":     m["referenceField"] != nil,
				},
			)
		}
	}
	for _, row := range rows {
		seen[row.Model] = true
		if row.Enabled {
			if mc, ok := toolsBindingConfig(row); ok {
				add(mc)
			}
		}
	}

	if caches := model.LoadModelCaches(); caches != nil {
		for _, mc := range caches.EnabledModelConfigsMap {
			if !seen[mc.Model] {
				add(mc)
			}
		}
	}

	sort.Slice(
		items,
		func(i, j int) bool {
			left, _ := items[i]["model"].(string)
			right, _ := items[j]["model"].(string)
			return left < right
		},
	)
	c.JSON(200, gin.H{"items": items})
}

func GetToolsModelAccess(c *gin.Context) {
	name := c.Query("model")
	if !toolsText(name, 191) {
		toolsError(c, 404, "model_not_found")
		return
	}

	mc, exists, err := resolveToolsModel(name)
	if err != nil {
		toolsError(c, 503, "model_route_unavailable")
		return
	}

	if !exists {
		toolsError(c, 404, "model_not_found")
		return
	}

	mapping, binding, _, ok := toolsDescriptor(mc)
	if !ok {
		toolsError(c, 503, "model_route_unavailable")
		return
	}

	revision, err := toolsRevision(mapping, binding)
	if err != nil {
		toolsError(c, 503, "model_route_unavailable")
		return
	}

	c.JSON(
		200,
		gin.H{
			"model":    name,
			"agent":    mapping,
			"contract": binding.Contract,
			"revision": revision,
		},
	)
}
