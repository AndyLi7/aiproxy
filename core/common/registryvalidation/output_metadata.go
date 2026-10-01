package registryvalidation

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

// OutputMetadataProjection is part of the immutable provider specification.
// It is not supplied by request parameters or inferred from response keys.
type OutputMetadataProjection struct {
	Version int      `json:"version"`
	Fields  []string `json:"fields"`
}

var outputMetadataName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
var privateOutputMetadataName = regexp.MustCompile(`(?i)(secret|password|credential|token|api_key|safety|nsfw|moderation|billing|cost|timing|request_id|file_|_url)`)

// V1 only covers declared scalar values and arrays thereof. Media, arbitrary
// maps and delivery/safety/billing fields require their own explicit handling.
func scalarMetadataSchema(schema map[string]any, depth int) bool {
	if depth > 8 || schema == nil {
		return false
	}
	for _, key := range []string{"$ref", "contentEncoding", "contentMediaType"} {
		if _, exists := schema[key]; exists {
			return false
		}
	}
	if format, _ := schema["format"].(string); strings.Contains(format, "uri") || strings.Contains(format, "url") {
		return false
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if raw, present := schema[key]; present {
			branches, ok := raw.([]any)
			if !ok || len(branches) < 2 || len(branches) > 16 {
				return false
			}
			for _, rawBranch := range branches {
				branch, ok := rawBranch.(map[string]any)
				if !ok || !scalarMetadataSchema(branch, depth+1) {
					return false
				}
			}
			return true
		}
	}
	switch schema["type"] {
	case "string", "number", "integer", "boolean", "null":
		return true
	case "array":
		items, ok := schema["items"].(map[string]any)
		return ok && scalarMetadataSchema(items, depth+1)
	default:
		return false
	}
}

func validateOutputMetadataProjection(spec *ProviderSpec) error {
	projection := spec.OutputMetadata
	if projection == nil {
		return nil
	}
	if (projection.Version != 1 && projection.Version != 2 && projection.Version != 3) || len(projection.Fields) == 0 || len(projection.Fields) > 64 {
		return ErrProviderContract
	}
	properties, ok := spec.Output["properties"].(map[string]any)
	if !ok {
		return ErrProviderContract
	}
	seen := map[string]bool{}
	for _, name := range projection.Fields {
		if !outputMetadataName.MatchString(name) || privateOutputMetadataName.MatchString(name) || seen[name] {
			return ErrProviderContract
		}
		switch name {
		case "image", "images", "seed", "seeds", "used_seed", "prompt", "actual_prompt", "revised_prompt", "description", "num_images":
			return ErrProviderContract
		}
		schema, ok := properties[name].(map[string]any)
		if !ok || (!scalarMetadataSchema(schema, 0) && !(projection.Version >= 2 && structuredMetadataSchema(name, schema)) && !(projection.Version == 3 && name == "best_info" && analysisDocumentArraySchema(schema))) {
			return ErrProviderContract
		}
		seen[name] = true
	}
	return nil
}

// ExtractFrozenProviderMetadata validates the complete provider result first,
// then copies only declared projected keys without converting JSON numbers.
func ExtractFrozenProviderMetadata(raw, output []byte) (map[string]json.RawMessage, error) {
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
	if provider.Upstream.OutputMetadata == nil {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(output, &fields) != nil {
		return nil, ErrProviderContract
	}
	result := map[string]json.RawMessage{}
	totalBytes := 0
	for _, name := range provider.Upstream.OutputMetadata.Fields {
		if value, present := fields[name]; present {
			if provider.Upstream.OutputMetadata.Version >= 2 {
				totalBytes += len(value)
				if totalBytes > 1048576 || !boundedMetadataJSON(value) {
					return nil, ErrProviderContract
				}
			}
			result[name] = append(json.RawMessage(nil), value...)
		}
	}
	return result, nil
}

// Structured documents are generated content, not arbitrary provider diagnostics.
func structuredMetadataSchema(name string, schema map[string]any) bool {
	if name != "structured_prompt" && name != "structured_instruction" && name != "vgl" {
		return false
	}
	if schema["type"] != "object" {
		return false
	}
	for _, key := range []string{"$ref", "contentEncoding", "contentMediaType"} {
		if _, ok := schema[key]; ok {
			return false
		}
	}
	return true
}

// Versioned transport bounds never truncate a generated document or convert numbers.
func boundedMetadataJSON(raw []byte) bool {
	if len(raw) > 1048576 {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	depth, nodes := 0, 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return depth == 0
		}
		if err != nil {
			return false
		}
		nodes++
		if nodes > 100000 {
			return false
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				depth++
				if depth > 32 {
					return false
				}
			case '}', ']':
				depth--
			}
		}
	}
}

// V3 adds the explicitly declared reflection document list; no diagnostic wildcard.
func analysisDocumentArraySchema(schema map[string]any) bool {
	for _, key := range []string{"anyOf", "oneOf"} {
		if raw, present := schema[key]; present {
			branches, ok := raw.([]any)
			if !ok || len(branches) != 2 {
				return false
			}
			nullBranch, arrayBranch := false, false
			for _, rawBranch := range branches {
				branch, ok := rawBranch.(map[string]any)
				if !ok {
					return false
				}
				nullBranch = nullBranch || branch["type"] == "null"
				arrayBranch = arrayBranch || (branch["type"] == "array" && analysisDocumentArraySchema(branch))
			}
			return nullBranch && arrayBranch
		}
	}
	items, ok := schema["items"].(map[string]any)
	return schema["type"] == "array" && ok && structuredMetadataSchema("structured_prompt", items)
}
