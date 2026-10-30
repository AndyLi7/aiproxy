package controller

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"sort"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
)

var (
	agentField    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
	agentID       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	agentUpstream = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./:-]{0,159}$`)
)

type agentModelInput struct {
	Kind            string   `json:"kind"`
	ID              string   `json:"id"`
	UpstreamModelID string   `json:"upstreamModelId"`
	MaxInputTokens  int      `json:"maxInputTokens"`
	MaxOutputTokens int      `json:"maxOutputTokens"`
	Vision          bool     `json:"vision"`
	InputUSD        float64  `json:"inputUSD"`
	OutputUSD       float64  `json:"outputUSD"`
	Sets            []string `json:"sets"`
	Model           string   `json:"model"`
}

// The limited Agent image tool consumes the existing public native contract.
// No provider schema or URL is authored by the operator in this workflow.
func agentImageMapping(mc model.ModelConfig) (map[string]any, bool) {
	// Capability is registry-owned metadata. A native task, a quote, an artifact
	// URL, or a model name alone says nothing about its output modality.
	capability, _ := mc.Config[model.ModelConfigCapabilityKey].(string)
	if capability != "text-to-image" && capability != "image-to-image" && capability != "edit" {
		return nil, false
	}

	binding, ok := parseNativeRouteBinding(mc)
	if !ok || mc.Type != mode.NativeTasks {
		return nil, false
	}

	quote, _, err := mc.ImagePrepaymentQuote()
	if err != nil || quote == nil {
		return nil, false
	}

	if _, err := nativeresult.CompileTaskContract(binding.Contract); err != nil {
		return nil, false
	}

	var contract nativeresult.TaskContract
	if json.Unmarshal(binding.Contract, &contract) != nil || len(contract.Artifacts) == 0 {
		return nil, false
	}

	var schema struct {
		Type       string   `json:"type"`
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type    string `json:"type"`
			Default any    `json:"default"`
		} `json:"properties"`
	}
	if json.Unmarshal(contract.InputSchema, &schema) != nil || schema.Type != "object" {
		return nil, false
	}
	// V1 supports only the unambiguous conventional public prompt field.
	prompt, ok := schema.Properties["prompt"]
	if !ok || prompt.Type != "string" {
		return nil, false
	}

	defaults := map[string]any{}

	var reference any

	format := "none"
	for field, spec := range schema.Properties {
		if !agentField.MatchString(field) {
			return nil, false
		}

		if field == "prompt" {
			continue
		}

		if field == "image_urls" {
			if !agentDataURIReferences(contract.InputSchema) ||
				(len(contract.UpstreamInputSchema) > 0 && !agentDataURIReferences(contract.UpstreamInputSchema)) {
				return nil, false
			}

			reference = field
			format = "data-uri-array"

			continue
		}

		if spec.Default != nil {
			switch v := spec.Default.(type) {
			case string:
				if len(v) > 1000 {
					return nil, false
				}
			case bool, float64:
			default:
				return nil, false
			}

			defaults[field] = spec.Default
		} else if slices.Contains(schema.Required, field) {
			return nil, false
		}
	}

	if capability != "text-to-image" && reference == nil {
		return nil, false
	}

	return map[string]any{
		"version":         1,
		"kind":            "image",
		"upstreamModelId": binding.Endpoint,
		"promptField":     "prompt",
		"referenceField":  reference,
		"referenceFormat": format,
		"defaults":        defaults,
	}, true
}

// V1 recognizes only explicit, simple data URI declarations. A plain string or
// format:uri also accepts HTTPS and does not prove the supplier accepts bytes.
// Unsupported schema compositions fail closed; never rewrite a frozen contract
// to make it eligible. Full request constraints still run before native dispatch.
func agentDataURIReferences(raw json.RawMessage) bool {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		return false
	}

	for key := range root {
		if !slices.Contains(
			[]string{
				"type",
				"properties",
				"required",
				"additionalProperties",
				"title",
				"description",
				"$schema",
			},
			key,
		) {
			return false
		}
	}

	var schema struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if json.Unmarshal(raw, &schema) != nil {
		return false
	}

	field := schema.Properties["image_urls"]
	if field["type"] != "array" {
		return false
	}

	for key := range field {
		if !slices.Contains(
			[]string{"type", "items", "minItems", "maxItems", "title", "description"},
			key,
		) {
			return false
		}
	}

	item, ok := field["items"].(map[string]any)
	if !ok || item["type"] != "string" {
		return false
	}

	for key := range item {
		if !slices.Contains(
			[]string{"type", "pattern", "minLength", "maxLength", "title", "description"},
			key,
		) {
			return false
		}
	}

	pattern, _ := item["pattern"].(string)
	switch pattern {
	case `^data:image/png;base64,[A-Za-z0-9+/=]+$`,
		`^data:image/jpeg;base64,[A-Za-z0-9+/=]+$`,
		`^data:image/webp;base64,[A-Za-z0-9+/=]+$`,
		`^data:image/(png|jpeg|webp);base64,[A-Za-z0-9+/=]+$`:
		return true
	default:
		return false
	}
}

func GetAgentModelOptions(c *gin.Context) {
	caches := model.LoadModelCaches()
	images := []map[string]any{}

	sets := []string{}
	for set := range caches.EnabledModelsBySet {
		sets = append(sets, set)
	}

	if len(sets) == 0 {
		sets = append(sets, model.ChannelDefaultSet)
	}

	sort.Strings(sets)

	for _, mc := range caches.EnabledModelConfigsMap {
		if mapping, ok := agentImageMapping(mc); ok {
			images = append(
				images,
				map[string]any{"model": mc.Model, "vision": mapping["referenceField"] != nil},
			)
		}
	}

	sort.Slice(
		images,
		func(i, j int) bool {
			left, _ := images[i]["model"].(string)
			right, _ := images[j]["model"].(string)
			return left < right
		},
	)
	middleware.SuccessResponse(c, gin.H{"images": images, "sets": sets})
}

func ConfigureAgentModel(c *gin.Context) {
	var in agentModelInput

	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 16000))
	decoder.DisallowUnknownFields()

	if decoder.Decode(&in) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		middleware.ErrorResponse(c, 400, "invalid Agent model")
		return
	}

	var mc model.ModelConfig
	switch in.Kind {
	case "inference":
		if !agentID.MatchString(in.ID) || !agentUpstream.MatchString(in.UpstreamModelID) ||
			in.MaxInputTokens < 1 ||
			in.MaxInputTokens > 1000000 ||
			in.MaxOutputTokens < 1200 ||
			in.MaxOutputTokens > 100000 ||
			in.InputUSD <= 0 ||
			in.OutputUSD <= 0 ||
			in.InputUSD > 1000000 ||
			in.OutputUSD > 1000000 ||
			math.IsNaN(in.InputUSD) ||
			math.IsNaN(in.OutputUSD) ||
			len(in.Sets) < 1 ||
			len(in.Sets) > 32 {
			middleware.ErrorResponse(c, 400, "invalid inference bounds or USD prices")
			return
		}

		caches := model.LoadModelCaches()
		for _, set := range in.Sets {
			if _, ok := caches.EnabledModelsBySet[set]; !ok && set != model.ChannelDefaultSet {
				middleware.ErrorResponse(c, 400, "unknown model set")
				return
			}
		}

		name := "agent-inference/" + in.ID
		descriptor := map[string]any{
			"version":                 1,
			"kind":                    "inference",
			"upstreamModelId":         in.UpstreamModelID,
			"maxInputTokens":          in.MaxInputTokens,
			"maxOutputTokens":         in.MaxOutputTokens,
			"vision":                  in.Vision,
			"toolCalling":             true,
			"imageTokensIncluded":     true,
			"reasoningTokensIncluded": true,
		}

		mc = model.ModelConfig{
			Model:            name,
			Owner:            "openrouter",
			Type:             mode.ChatCompletions,
			ExcludeFromTests: true,
			Price: model.Price{
				InputPrice:      model.ZeroNullFloat64(in.InputUSD),
				InputPriceUnit:  1000000,
				OutputPrice:     model.ZeroNullFloat64(in.OutputUSD),
				OutputPriceUnit: 1000000,
			},
			Config: map[model.ModelConfigKey]any{
				model.AgentExecutionKey:     descriptor,
				model.AgentVisibilityKey:    "private",
				model.AgentInferenceSetsKey: in.Sets,
				"x_token_platform_pricing": map[string]any{
					"currency":        "USD",
					"pricing_version": "agent-" + in.ID,
				},
			},
		}
		if err := model.CreateAgentInferenceProduct(mc); err != nil {
			middleware.ErrorResponse(c, 409, "Agent model already exists or configuration changed")
			return
		}
	case "image":
		var err error

		mc, err = model.GetModelConfig(in.Model)
		if err != nil {
			middleware.ErrorResponse(c, 404, "model unavailable")
			return
		}

		if _, ok := model.LoadModelCaches().EnabledModelConfigsMap[in.Model]; !ok {
			middleware.ErrorResponse(c, 400, "model has no enabled route")
			return
		}

		mapping, ok := agentImageMapping(mc)
		if !ok {
			middleware.ErrorResponse(c, 400, "unsupported image contract or missing quote")
			return
		}

		if err := model.AttachAgentMapping(mc, mapping); err != nil {
			middleware.ErrorResponse(c, 409, "model changed; refresh selection")
			return
		}
	default:
		middleware.ErrorResponse(c, 400, "invalid kind")
		return
	}

	middleware.SuccessResponse(c, gin.H{"model": mc.Model})
}
