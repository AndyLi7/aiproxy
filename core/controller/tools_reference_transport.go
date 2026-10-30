package controller

import (
	"encoding/json"

	"github.com/labring/aiproxy/core/common/nativeresult"
)

// Fal's file protocol accepts base64 data URIs in image file inputs. Official
// OpenAPI schemas identify those leaves with _fal_ui_field=image, without a
// data-URI-only regex. This adapter is internal Tools only; toolsDescriptor also
// requires an enabled Fal channel and the standard queue origin. Public API
// admission keeps agentDataURIReferences and its existing strict declarations.
// Full public/upstream schemas are validated here and again on every submission.
// No schema is rewritten, no default is persisted, and no provider is contacted.
func toolsFalDataURIReferences(raw json.RawMessage) bool {
	var contract nativeresult.TaskContract
	if json.Unmarshal(raw, &contract) != nil {
		return false
	}

	var root map[string]any
	if json.Unmarshal(contract.InputSchema, &root) != nil || root["type"] != "object" {
		return false
	}

	properties, _ := root["properties"].(map[string]any)
	field, _ := properties["image_urls"].(map[string]any)

	item, _ := field["items"].(map[string]any)
	if field["type"] != "array" || item["type"] != "string" ||
		(item["_fal_ui_field"] != "image" && field["_fal_ui_field"] != "image") {
		return false
	}

	input := map[string]any{
		"prompt":     "Image reference transport validation",
		"image_urls": []string{toolsPNGReferenceProbe},
	}
	for key, value := range properties {
		property, _ := value.(map[string]any)

		if _, fixed := contract.FixedParameters[key]; fixed {
			continue
		}

		if key != "prompt" && key != "image_urls" && property["default"] != nil {
			input[key] = property["default"]
		}
	}
	// Let the native validator resolve all schema references/compositions, including
	// the upstream schema. The file annotation is capability metadata, not a bypass.
	compiled, err := nativeresult.CompileTaskContract(raw)
	if err != nil {
		return false
	}

	request, err := json.Marshal(map[string]any{"model": contract.Model, "input": input})
	if err != nil {
		return false
	}

	validated, err := compiled.ValidateRequest(request)
	if err != nil {
		return false
	}

	_, err = compiled.PrepareUpstreamInput(validated)

	return err == nil
}

// Used only for local schema validation, never dispatched or stored as an asset.
const toolsPNGReferenceProbe = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jN1sAAAAASUVORK5CYII="
