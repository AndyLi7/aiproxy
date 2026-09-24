// Package registryvalidation validates requests against server-owned registry contracts.
package registryvalidation

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"maps"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type ValidationError struct {
	Message  string
	Code     string
	Status   int
	Param    string
	Expected map[string]any
}

func (e *ValidationError) PublicError() map[string]any {
	var param any
	if e.Param != "" {
		param = e.Param
	}
	result := map[string]any{"message": e.Error(), "type": "invalid_request_error", "code": "invalid_parameter", "param": param}
	if e.Code != "" {
		result["code"] = e.Code
	}
	if len(e.Expected) > 0 {
		result["expected"] = e.Expected
		if values, ok := e.Expected["enum"]; ok {
			result["allowed_values"] = values
		}
	}
	return result
}

// Only emit constraints from the public schema; never echo submitted values,
// raw validator errors, schema URLs or provider metadata.
func schemaInputError(err error, schema map[string]any) *ValidationError {
	result := &ValidationError{Status: 400}
	var validation *jsonschema.ValidationError
	if !errors.As(err, &validation) {
		return result
	}
	for len(validation.Causes) > 0 {
		validation = validation.Causes[0]
	}
	path := append([]string{}, validation.InstanceLocation...)
	if required, ok := validation.ErrorKind.(*kind.Required); ok && len(required.Missing) > 0 {
		path = append(path, required.Missing[0])
	}
	if extra, ok := validation.ErrorKind.(*kind.AdditionalProperties); ok && len(extra.Properties) > 0 {
		path = append(path, extra.Properties[0])
		return &ValidationError{Status: 400, Param: strings.Join(path, "."), Message: "This parameter is not supported by the selected model.", Expected: map[string]any{"allowed": false}}
	}
	node := schema
	for _, part := range path {
		properties, _ := node["properties"].(map[string]any)
		if child, ok := properties[part].(map[string]any); ok {
			node = child
		} else if child, ok := node["items"].(map[string]any); ok {
			node = child
		} else {
			node = nil
			break
		}
	}
	result.Param = strings.Join(path, ".")
	result.Expected = map[string]any{}
	for _, key := range []string{"type", "enum", "const", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "minLength", "maxLength", "minItems", "maxItems", "format"} {
		if value, ok := node[key]; ok {
			result.Expected[key] = value
		}
	}
	return result
}

func (e *ValidationError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Status == 400 {
		if e.Param != "" {
			return "Invalid parameter: " + e.Param + ". See expected constraints."
		}
		return "Image request does not satisfy the model schema"
	}
	return "Image model validation contract is unavailable"
}

type denyLoader struct{}

func (denyLoader) Load(string) (any, error) {
	return nil, errors.New("external schema references disabled")
}

type imageRules struct {
	Version    int `json:"version"`
	Dimensions *struct {
		Presets   []string `json:"presets"`
		MinPixels float64  `json:"minPixels"`
		MaxPixels float64  `json:"maxPixels"`
		MinRatio  float64  `json:"minRatio"`
		MaxRatio  float64  `json:"maxRatio"`
	} `json:"dimensions"`
	CombinedImages *struct {
		Maximum int `json:"maximum"`
	} `json:"combinedImages"`
}

var dimensionsPattern = regexp.MustCompile(`^([1-9][0-9]*)x([1-9][0-9]*)$`)

// ValidateImage returns the exact normalized body that must be forwarded upstream.
// Contract bytes must come from trusted server configuration, never request input.
func ValidateImage(contract []byte, publicID string, body []byte) ([]byte, *ValidationError) {
	unavailable := &ValidationError{Status: 503}
	invalid := &ValidationError{Status: 400}

	if len(contract) > 256*1024 {
		return nil, unavailable
	}

	var entry struct {
		ID              string          `json:"entry_id"`
		Version         int             `json:"validation_version"`
		ProviderVersion json.RawMessage `json:"provider_contract_version"`
		Schema          map[string]any  `json:"input_schema"`
		Providers       map[string]struct {
			Fixed map[string]any `json:"fixedParameters"`
		} `json:"providers"`
	}
	if json.Unmarshal(contract, &entry) != nil || publicID == "" || entry.ID != publicID ||
		entry.Version != 1 ||
		entry.Schema == nil {
		return nil, unavailable
	}

	if entry.ProviderVersion != nil && string(entry.ProviderVersion) != "1" {
		return nil, unavailable
	}

	rules, ok := parseImageRules(entry.Schema)
	if !ok {
		return nil, unavailable
	}

	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(denyLoader{})
	compiler.AssertFormat()

	const resource = "https://registry.invalid/input.json"
	if compiler.AddResource(resource, entry.Schema) != nil {
		return nil, unavailable
	}

	schema, err := compiler.Compile(resource)
	if err != nil {
		return nil, unavailable
	}

	var input map[string]any
	if json.Unmarshal(body, &input) != nil || input == nil || input["model"] != publicID {
		return nil, invalid
	}

	delete(input, "model")
	// Provider transport constants are not public schema fields. Accept only
	// their exact registered values, and forward the same constants afterwards.
	var fixed map[string]any

	first := true
	for _, provider := range entry.Providers {
		if entry.ProviderVersion != nil {
			break
		}

		if first {
			fixed = provider.Fixed
			first = false
		} else if !reflect.DeepEqual(fixed, provider.Fixed) {
			return nil, unavailable
		}
	}

	properties, _ := entry.Schema["properties"].(map[string]any)
	for key, value := range fixed {
		if _, exists := properties[key]; exists || key == "model" {
			return nil, unavailable
		}

		if supplied, exists := input[key]; exists && !reflect.DeepEqual(supplied, value) {
			return nil, invalid
		}

		delete(input, key)
	}
	// OpenAI-style clients send size as WIDTHxHEIGHT. A capability that
	// publishes image_size may accept it as an alias; validate the translated
	// value against that capability's frozen schema before dispatch.
	if rawSize, supplied := input["size"]; supplied && properties["size"] == nil && properties["image_size"] != nil {
		if _, duplicate := input["image_size"]; duplicate {
			return nil, &ValidationError{Status: 400, Param: "size", Message: "Provide either size or image_size, not both."}
		}
		value, stringValue := rawSize.(string)
		if !stringValue {
			return nil, &ValidationError{Status: 400, Param: "size", Expected: map[string]any{"type": "string"}}
		}
		if dimensions := dimensionsPattern.FindStringSubmatch(value); dimensions != nil {
			width, widthErr := strconv.ParseInt(dimensions[1], 10, 64)
			height, heightErr := strconv.ParseInt(dimensions[2], 10, 64)
			if widthErr != nil || heightErr != nil {
				return nil, &ValidationError{Status: 400, Param: "size", Expected: map[string]any{"format": "WIDTHxHEIGHT"}}
			}
			input["image_size"] = map[string]any{"width": width, "height": height}
		} else {
			input["image_size"] = value
		}
		delete(input, "size")
	}

	applyDefaults(input, entry.Schema)

	if err := schema.Validate(input); err != nil {
		return nil, schemaInputError(err, entry.Schema)
	}
	// Old published contracts can require prompt without a minLength. Reject a
	// blank prompt before a paid synchronous or asynchronous submission.
	if required, ok := entry.Schema["required"].([]any); ok {
		for _, name := range required {
			if name == "prompt" {
				if prompt, isString := input["prompt"].(string); isString && strings.TrimSpace(prompt) == "" {
					return nil, &ValidationError{Status: 400, Param: "prompt", Expected: map[string]any{"minLength": 1}}
				}
			}
		}
	}

	if !validateImageConstraints(input, rules) {
		if d := rules.Dimensions; d != nil {
			dimensionOnly := imageRules{Dimensions: d}
			if !validateImageConstraints(input, dimensionOnly) {
				return nil, &ValidationError{Status: 400, Param: "size", Expected: dimensionExpectations(rules)}
			}
		}
		return nil, &ValidationError{Status: 400, Param: "sequential_image_generation_options.max_images", Expected: map[string]any{"maximum_combined_images": rules.CombinedImages.Maximum}}
	}

	input["model"] = publicID
	maps.Copy(input, fixed)

	normalized, err := json.Marshal(input)
	if err != nil {
		return nil, invalid
	}

	return normalized, nil
}

func applyDefaults(input, schema map[string]any) {
	properties, _ := schema["properties"].(map[string]any)
	for key, raw := range properties {
		property, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		if _, exists := input[key]; !exists {
			if value, ok := property["default"]; ok {
				input[key] = value
			}
		}

		if nested, ok := input[key].(map[string]any); ok {
			applyDefaults(nested, property)
		}
	}
}

func parseImageRules(schema map[string]any) (imageRules, bool) {
	var rules imageRules
	if extension, ok := schema["x-image-constraints"]; ok {
		raw, err := json.Marshal(extension)
		if err != nil {
			return imageRules{}, false
		}

		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()

		if decoder.Decode(&rules) != nil || rules.Version != 1 {
			return imageRules{}, false
		}

		if d := rules.Dimensions; d != nil {
			if len(d.Presets) == 0 || d.MinPixels <= 0 || d.MaxPixels < d.MinPixels ||
				d.MaxPixels > 9007199254740991 ||
				math.Trunc(d.MinPixels) != d.MinPixels ||
				math.Trunc(d.MaxPixels) != d.MaxPixels ||
				d.MinRatio <= 0 ||
				d.MaxRatio < d.MinRatio {
				return imageRules{}, false
			}
		}

		if c := rules.CombinedImages; c != nil && (c.Maximum < 2 || c.Maximum > 100) {
			return imageRules{}, false
		}
	}

	return rules, true
}

func validateImageConstraints(input map[string]any, rules imageRules) bool {
	if d := rules.Dimensions; d != nil {
		size, ok := input["size"].(string)
		if !ok {
			return false
		}

		preset := false
		for _, p := range d.Presets {
			if p == size {
				preset = true
			}
		}

		if !preset {
			match := dimensionsPattern.FindStringSubmatch(size)
			if match == nil {
				return false
			}

			w, e1 := strconv.ParseFloat(match[1], 64)
			h, e2 := strconv.ParseFloat(match[2], 64)

			pixels := w * h
			if e1 != nil || e2 != nil || math.IsInf(pixels, 0) || pixels > d.MaxPixels ||
				pixels < d.MinPixels ||
				w/h < d.MinRatio ||
				w/h > d.MaxRatio {
				return false
			}
		}
	}

	if c := rules.CombinedImages; c != nil && input["sequential_image_generation"] == "auto" {
		count := 0
		switch images := input["image"].(type) {
		case string:
			count = 1
		case []any:
			count = len(images)
		}

		maximum := float64(1)
		if options, ok := input["sequential_image_generation_options"].(map[string]any); ok {
			if value, ok := options["max_images"].(float64); ok {
				maximum = value
			}
		}

		if float64(count)+maximum > float64(c.Maximum) {
			return false
		}
	}

	return true
}
