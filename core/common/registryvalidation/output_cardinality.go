package registryvalidation

import (
	"encoding/json"
	"math"
)

// Cardinality comes from frozen provider metadata, never request-authored code.
func resolveOutputCardinality(raw json.RawMessage, schema, body map[string]any, limit int, output ...map[string]any) (int, error) {
	var d map[string]any
	if json.Unmarshal(raw, &d) != nil || d == nil {
		return 0, ErrProviderContract
	}
	if d["version"] == float64(3) {
		return resolveConditionalOutputCount(d, schema, body, limit)
	}
	if d["version"] == float64(2) {
		if len(output) != 1 || len(d) != 3 || d["mode"] != "bounded" {
			return 0, ErrProviderContract
		}
		projection, err := InferLayeredImageProjection(output[0])
		if err != nil || d["maximum"] != float64(projection.Maximum) || projection.Maximum > limit {
			return 0, ErrProviderContract
		}
		return projection.Maximum, nil
	}
	if d["version"] != float64(1) {
		return 0, ErrProviderContract
	}
	mode, _ := d["mode"].(string)
	integer := func(v any) (int, bool) {
		n, ok := v.(float64)
		if !ok || n < 1 || n > 15 || math.Trunc(n) != n {
			return 0, false
		}
		return int(n), true
	}
	if mode == "fixed" {
		count, ok := integer(d["count"])
		if len(d) != 3 || !ok || count > limit {
			return 0, ErrProviderContract
		}
		return count, nil
	}
	if mode != "parameter" && mode != "array-length" {
		return 0, ErrProviderContract
	}
	maximum, ok := integer(d["maximum"])
	parameter, pok := d["parameter"].(string)
	if len(d) != 4 || !ok || !pok || !meteringParameter.MatchString(parameter) {
		return 0, ErrProviderContract
	}
	props, _ := schema["properties"].(map[string]any)
	field, _ := props[parameter].(map[string]any)
	count := 0
	if mode == "parameter" {
		minimum, minOK := integer(field["minimum"])
		if field["type"] != "integer" || !minOK || minimum > maximum || field["maximum"] != float64(maximum) {
			return 0, ErrProviderContract
		}
		count, ok = integer(body[parameter])
		if !ok || count < minimum {
			return 0, ErrProviderContract
		}
	} else {
		values, ok := body[parameter].([]any)
		if field["type"] != "array" || !ok {
			return 0, ErrProviderContract
		}
		count = len(values)
	}
	if count < 1 || count > maximum || count > limit {
		return 0, ErrProviderContract
	}
	return count, nil
}
