package registryvalidation

import (
	"encoding/json"
	"math"
)

func conditionalCountRule(raw json.RawMessage) string {
	var d map[string]any
	if json.Unmarshal(raw, &d) != nil || d["version"] != float64(3) || d["mode"] != "conditional" ||
		len(d) != 4 {
		return ""
	}

	rule, _ := d["rule"].(string)

	return rule
}

func resolveConditionalOutputCount(d, schema, body map[string]any, limit int) (int, error) {
	fail := func() (int, error) { return 0, ErrProviderContract }
	p, _ := schema["properties"].(map[string]any)
	field := func(key string) map[string]any { v, _ := p[key].(map[string]any); return v }

	if len(d) != 4 || d["mode"] != "conditional" {
		return fail()
	}

	var result int
	switch d["rule"] {
	case "product-placement":
		if d["maximum"] != float64(40) || !conditionalInteger(field("num_results"), 1, 4) ||
			field("num_results")["description"] != "The number of lifestyle product shots you would like to generate. You will get num_results x 10 results when placement_type=automatic and according to the number of required placements x num_results if placement_type=manual_placement." ||
			!conditionalChoices(
				field("placement_type"),
				"original",
				"automatic",
				"manual_placement",
				"manual_padding",
			) ||
			!conditionalChoices(
				field("manual_placement_selection"),
				"upper_left",
				"upper_right",
				"bottom_left",
				"bottom_right",
				"right_center",
				"left_center",
				"upper_center",
				"bottom_center",
				"center_vertical",
				"center_horizontal",
			) {
			return fail()
		}

		n, ok := conditionalCount(body["num_results"], 1, 4)
		if !ok {
			return fail()
		}

		result = n
		switch body["placement_type"] {
		case "automatic":
			result *= 10
		case "original", "manual_placement", "manual_padding":
		default:
			return fail()
		}
	case "image-series":
		if d["maximum"] != float64(9) ||
			!conditionalChoices(field("result_type"), "single", "series") ||
			!conditionalInteger(field("num_images"), 1, 9) ||
			field("num_images")["description"] != "Number of images to generate (1-9). Only used when result_type is 'single'." ||
			field("series_amount")["description"] != "Number of images in series (2-9). Only used when result_type is 'series'." {
			return fail()
		}

		if !conditionalNullableSeriesCount(field("series_amount")) {
			return fail()
		}

		switch body["result_type"] {
		case "single":
			n, ok := conditionalCount(body["num_images"], 1, 9)
			if !ok {
				return fail()
			}

			result = n
		case "series":
			result = 9
			if body["series_amount"] != nil {
				n, ok := conditionalCount(body["series_amount"], 2, 9)
				if !ok {
					return fail()
				}

				result = n
			}
		default:
			return fail()
		}
	default:
		return fail()
	}

	if result < 1 || result > limit {
		return fail()
	}

	return result, nil
}

func conditionalInteger(v map[string]any, minimum, maximum int) bool {
	return v["type"] == "integer" && v["minimum"] == float64(minimum) &&
		v["maximum"] == float64(maximum)
}

func conditionalChoices(v map[string]any, want ...string) bool {
	values, ok := v["enum"].([]any)
	if !ok || v["type"] != "string" || len(values) != len(want) {
		return false
	}

	seen := map[string]bool{}
	for _, x := range values {
		s, ok := x.(string)
		if !ok || seen[s] {
			return false
		}

		seen[s] = true
	}

	for _, s := range want {
		if !seen[s] {
			return false
		}
	}

	return true
}

func conditionalCount(value any, minimum, maximum int) (int, bool) {
	n, ok := value.(float64)
	return int(n), ok && n >= float64(minimum) && n <= float64(maximum) && math.Trunc(n) == n
}

func conditionalNullableSeriesCount(schema map[string]any) bool {
	variants, ok := schema["anyOf"].([]any)
	if !ok || len(variants) != 2 {
		return false
	}

	hasNull, hasCount := false, false
	for _, v := range variants {
		f, _ := v.(map[string]any)
		hasNull = hasNull || f["type"] == "null"
		hasCount = hasCount || conditionalInteger(f, 2, 9)
	}

	if !hasNull || !hasCount {
		return false
	}

	return true
}
