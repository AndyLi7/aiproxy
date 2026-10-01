package registryvalidation

import "encoding/json"

// FrozenImageMapTypeEnabled permits only source-declared PBR image identities.
// Unknown fields on other provider responses do not become public metadata.
func FrozenImageMapTypeEnabled(raw []byte) bool {
	if !HasProviderContracts(raw) {
		return false
	}
	var frozen struct {
		Binding ProviderBinding `json:"selected_provider_binding"`
	}
	if json.Unmarshal(raw, &frozen) != nil {
		return false
	}
	provider, err := bound(raw, frozen.Binding)
	if err != nil {
		return false
	}
	props, _ := provider.Upstream.Output["properties"].(map[string]any)
	images, _ := props["images"].(map[string]any)
	item, _ := images["items"].(map[string]any)
	fields, _ := item["properties"].(map[string]any)
	field, _ := fields["map_type"].(map[string]any)
	values, _ := field["enum"].([]any)
	if field["type"] != "string" || len(values) == 0 {
		return false
	}
	for _, value := range values {
		switch value {
		case "basecolor", "normal", "roughness", "metalness", "height":
		default:
			return false
		}
	}
	return true
}
