// Package registryvalidation validates requests against server-owned registry contracts.
package registryvalidation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type ValidationError struct{ Status int }

func (e *ValidationError) Error() string {
	if e.Status == 400 {
		return "Image request does not satisfy the model schema"
	}
	return "Image model validation contract is unavailable"
}

type denyLoader struct{}

func (denyLoader) Load(string) (any, error) {
	return nil, fmt.Errorf("external schema references disabled")
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
		ID        string         `json:"entry_id"`
		Version   int            `json:"validation_version"`
		Schema    map[string]any `json:"input_schema"`
		Providers map[string]struct {
			Fixed map[string]any `json:"fixedParameters"`
		} `json:"providers"`
	}
	if json.Unmarshal(contract, &entry) != nil || publicID == "" || entry.ID != publicID || entry.Version != 1 || entry.Schema == nil {
		return nil, unavailable
	}
	var rules imageRules
	if extension, ok := entry.Schema["x-image-constraints"]; ok {
		raw, err := json.Marshal(extension)
		if err != nil {
			return nil, unavailable
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&rules) != nil || rules.Version != 1 {
			return nil, unavailable
		}
		if d := rules.Dimensions; d != nil {
			if len(d.Presets) == 0 || d.MinPixels <= 0 || d.MaxPixels < d.MinPixels || d.MaxPixels > 9007199254740991 || math.Trunc(d.MinPixels) != d.MinPixels || math.Trunc(d.MaxPixels) != d.MaxPixels || d.MinRatio <= 0 || d.MaxRatio < d.MinRatio {
				return nil, unavailable
			}
		}
		if c := rules.CombinedImages; c != nil && (c.Maximum < 2 || c.Maximum > 100) {
			return nil, unavailable
		}
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
	applyDefaults(input, entry.Schema)
	if schema.Validate(input) != nil {
		return nil, invalid
	}
	if d := rules.Dimensions; d != nil {
		size, ok := input["size"].(string)
		if !ok {
			return nil, invalid
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
				return nil, invalid
			}
			w, e1 := strconv.ParseFloat(match[1], 64)
			h, e2 := strconv.ParseFloat(match[2], 64)
			pixels := w * h
			if e1 != nil || e2 != nil || math.IsInf(pixels, 0) || pixels > d.MaxPixels || pixels < d.MinPixels || w/h < d.MinRatio || w/h > d.MaxRatio {
				return nil, invalid
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
			return nil, invalid
		}
	}
	input["model"] = publicID
	for key, value := range fixed {
		input[key] = value
	}
	normalized, err := json.Marshal(input)
	if err != nil {
		return nil, invalid
	}
	return normalized, nil
}

func applyDefaults(input map[string]any, schema map[string]any) {
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
