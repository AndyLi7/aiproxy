package registryvalidation

import (
	"encoding/json"
	"errors"
)

var ErrUnsafeImageResult = errors.New("unsafe image result")

// Internal provider controls never become customer metadata. Only frozen source
// declarations enable processing; arbitrary response keys cannot invent controls.
func CheckFrozenOutputControls(raw, result []byte, images int) error {
	if !HasProviderContracts(raw) {
		return nil
	}
	var frozen struct {
		Binding ProviderBinding `json:"selected_provider_binding"`
	}
	if json.Unmarshal(raw, &frozen) != nil {
		return ErrProviderContract
	}
	p, err := bound(raw, frozen.Binding)
	if err != nil {
		return err
	}
	props, _ := p.Upstream.Output["properties"].(map[string]any)
	var fields map[string]json.RawMessage
	if json.Unmarshal(result, &fields) != nil {
		return ErrProviderContract
	}
	for _, name := range []string{"nsfw_content_detected", "has_nsfw_concepts"} {
		schema, _ := props[name].(map[string]any)
		items, _ := schema["items"].(map[string]any)
		if schema["type"] != "array" || items["type"] != "boolean" {
			continue
		}
		value, present := fields[name]
		if !present {
			continue
		} // Required fields were validated against the frozen schema.
		var flags []bool
		if json.Unmarshal(value, &flags) != nil || len(flags) != images {
			return ErrProviderContract
		}
		for _, flag := range flags {
			if flag {
				return ErrUnsafeImageResult
			}
		}
	}
	return nil
}
