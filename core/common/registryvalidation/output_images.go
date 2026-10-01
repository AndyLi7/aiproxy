package registryvalidation

import (
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
)

type OutputImageProjection struct {
	Version int      `json:"version"`
	Maximum int      `json:"maximum,omitempty"`
	Mode    string   `json:"mode"`
	Fields  []string `json:"fields,omitempty"`
}

func typedImage(schema map[string]any) bool {
	if schema["type"] != "object" {
		return false
	}
	props, _ := schema["properties"].(map[string]any)
	url, _ := props["url"].(map[string]any)
	if url["type"] != "string" {
		return false
	}
	required, _ := schema["required"].([]any)
	for _, v := range required {
		if v == "url" {
			return true
		}
	}
	return false
}
func validateOutputImageProjection(spec *ProviderSpec) error {
	p := spec.OutputImages
	if p == nil {
		return nil
	}
	if p.Version == 3 {
		expected, err := InferLayeredImageProjection(spec.Output)
		if err != nil || p.Mode != expected.Mode || p.Maximum != expected.Maximum || len(p.Fields) != 0 {
			return ErrProviderContract
		}
		return nil
	}
	if p.Maximum != 0 {
		return ErrProviderContract
	}
	if p.Version == 2 {
		return validateNamedImages(spec)
	}
	if len(p.Fields) != 0 {
		return ErrProviderContract
	}
	if spec.Output["type"] != "object" || p.Version != 1 || p.Mode != "merge-identical" {
		return ErrProviderContract
	}
	props, _ := spec.Output["properties"].(map[string]any)
	single, _ := props["image"].(map[string]any)
	array, _ := props["images"].(map[string]any)
	item, _ := array["items"].(map[string]any)
	if !nullableTypedImage(single) || array["type"] != "array" || !typedImage(item) {
		return ErrProviderContract
	}
	required, _ := spec.Output["required"].([]any)
	for _, v := range required {
		if v == "image" {
			return nil
		}
	}
	return ErrProviderContract
}
func FrozenCombinedImageOutputs(raw []byte) (bool, error) {
	if !HasProviderContracts(raw) {
		return false, nil
	}
	var frozen struct {
		Binding ProviderBinding `json:"selected_provider_binding"`
	}
	if json.Unmarshal(raw, &frozen) != nil {
		return false, ErrProviderContract
	}
	p, err := bound(raw, frozen.Binding)
	if err != nil {
		return false, err
	}
	return p.Upstream.OutputImages != nil && p.Upstream.OutputImages.Version == 1, nil
}

func nullableTypedImage(schema map[string]any) bool {
	if typedImage(schema) {
		return true
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		branches, ok := schema[key].([]any)
		if !ok || len(branches) != 2 {
			continue
		}
		nulls, images := 0, 0
		for _, raw := range branches {
			b, _ := raw.(map[string]any)
			if b["type"] == "null" {
				nulls++
			} else if typedImage(b) {
				images++
			}
		}
		if nulls == 1 && images == 1 {
			return true
		}
	}
	return false
}

var namedImageField = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func validateNamedImages(spec *ProviderSpec) error {
	p := spec.OutputImages
	props, _ := spec.Output["properties"].(map[string]any)
	required, _ := spec.Output["required"].([]any)
	if spec.Output["type"] != "object" || p.Mode != "named" || props["image"] != nil || props["images"] != nil {
		return ErrProviderContract
	}
	expected := []string{}
	for name, raw := range props {
		s, _ := raw.(map[string]any)
		if s["type"] != "object" || s["description"] != "Represents an image file." {
			continue
		}
		if !namedImageField.MatchString(name) || !typedImage(s) {
			return ErrProviderContract
		}
		found := false
		for _, r := range required {
			if r == name {
				found = true
			}
		}
		if !found {
			return ErrProviderContract
		}
		fields, _ := s["properties"].(map[string]any)
		for key := range fields {
			switch key {
			case "url", "content_type", "width", "height", "file_name", "file_size":
			default:
				return ErrProviderContract
			}
		}
		needed, _ := s["required"].([]any)
		for _, r := range needed {
			key, ok := r.(string)
			if !ok || fields[key] == nil {
				return ErrProviderContract
			}
		}
		expected = append(expected, name)
	}
	sort.Strings(expected)
	if len(expected) == 0 || len(expected) > 15 || !reflect.DeepEqual(expected, p.Fields) {
		return ErrProviderContract
	}
	return nil
}
func FrozenNamedImageOutputs(raw []byte) ([]string, error) {
	if !HasProviderContracts(raw) {
		return nil, nil
	}
	var frozen struct {
		Binding ProviderBinding `json:"selected_provider_binding"`
	}
	if json.Unmarshal(raw, &frozen) != nil {
		return nil, ErrProviderContract
	}
	p, err := bound(raw, frozen.Binding)
	if err != nil {
		return nil, err
	}
	projection := p.Upstream.OutputImages
	if projection == nil || projection.Version != 2 {
		return nil, nil
	}
	return append([]string(nil), projection.Fields...), nil
}
