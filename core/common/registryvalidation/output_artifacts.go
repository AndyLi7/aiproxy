package registryvalidation

import (
	"bytes"
	"encoding/json"
)

// OutputArtifactProjection is immutable provider configuration, not request input.
type OutputArtifactProjection struct {
	Version int      `json:"version"`
	Primary string   `json:"primary"`
	Fields  []string `json:"fields"`
}

func artifactImageSchema(schema map[string]any) bool {
	if schema["type"] == "object" {
		if !typedImage(schema) {
			return false
		}
		props, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, rawName := range required {
			name, ok := rawName.(string)
			if !ok {
				return false
			}
			if _, exists := props[name]; !exists {
				return false
			}
		}
		for name := range props {
			switch name {
			case "url", "width", "height", "content_type", "file_name", "file_size":
			default:
				return false
			}
		}
		return true
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		branches, ok := schema[key].([]any)
		if !ok || len(branches) != 2 {
			continue
		}
		nulls, images := 0, 0
		for _, b := range branches {
			node, _ := b.(map[string]any)
			if node["type"] == "null" {
				nulls++
			} else if node["type"] == "object" && artifactImageSchema(node) {
				images++
			}
		}
		if nulls == 1 && images == 1 {
			return true
		}
	}
	return false
}
func validateOutputArtifactProjection(spec *ProviderSpec) error {
	p := spec.OutputArtifacts
	if p == nil {
		return nil
	}
	if (p.Version != 1 && p.Version != 2 && p.Version != 3) || p.Primary != "image" || len(p.Fields) != 1 || (p.Version == 1 && p.Fields[0] != "mask_image") || (p.Version == 2 && p.Fields[0] != "mask") || (p.Version == 3 && p.Fields[0] != "transparent_overlay") || spec.Output["type"] != "object" {
		return ErrProviderContract
	}
	props, _ := spec.Output["properties"].(map[string]any)
	if _, exists := props["images"]; exists {
		return ErrProviderContract
	}
	image, _ := props["image"].(map[string]any)
	if _, exists := props["mask_image"]; p.Version == 2 && exists {
		return ErrProviderContract
	}
	if p.Version == 3 {
		for _, name := range []string{"mask", "mask_image"} {
			if _, exists := props[name]; exists {
				return ErrProviderContract
			}
		}
	}
	mask, _ := props[p.Fields[0]].(map[string]any)
	if !artifactImageSchema(image) || !artifactImageSchema(mask) {
		return ErrProviderContract
	}
	required, _ := spec.Output["required"].([]any)
	for _, name := range required {
		if name == "image" {
			return nil
		}
	}
	return ErrProviderContract
}
func ExtractFrozenAuxiliaryImages(raw, output []byte) (map[string]json.RawMessage, error) {
	if err := ValidateFrozenProviderOutput(raw, output); err != nil {
		return nil, err
	}
	if !HasProviderContracts(raw) {
		return nil, nil
	}
	var frozen struct {
		Binding ProviderBinding `json:"selected_provider_binding"`
	}
	if json.Unmarshal(raw, &frozen) != nil {
		return nil, ErrProviderContract
	}
	provider, err := bound(raw, frozen.Binding)
	if err != nil {
		return nil, err
	}
	if provider.Upstream.OutputArtifacts == nil {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(output, &fields) != nil {
		return nil, ErrProviderContract
	}
	result := map[string]json.RawMessage{}
	for _, name := range provider.Upstream.OutputArtifacts.Fields {
		if value, present := fields[name]; present {
			target := name
			if provider.Upstream.OutputArtifacts.Version == 2 {
				target = "mask_image"
			}
			result[target] = bytes.Clone(value)
		}
	}
	return result, nil
}
