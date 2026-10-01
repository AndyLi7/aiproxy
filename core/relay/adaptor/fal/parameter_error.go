package fal

import (
	"fmt"
	"github.com/labring/aiproxy/core/model"
	"math"
	"regexp"
	"strings"
)

var parameterSegment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
var privateParameter = regexp.MustCompile(`(?i)token|secret|password|credential|authorization|api_key|fal|provider|upstream`)

// Only structured locations and known rule codes cross the public boundary.
// Never copy msg, input, ctx, exception text, URLs or upstream response bodies.
func parameterIssue(kind string, loc []any) (model.ImageParameterIssue, bool) {
	rules := map[string]string{
		"missing": "required", "string_type": "string", "int_type": "integer", "int_parsing": "integer",
		"float_type": "number", "float_parsing": "number", "bool_type": "boolean", "bool_parsing": "boolean",
		"list_type": "array", "dict_type": "object", "literal_error": "allowed_value", "enum": "allowed_value",
		"greater_than": "range", "greater_than_equal": "range", "less_than": "range", "less_than_equal": "range",
		"too_short": "length", "too_long": "length", "string_too_short": "length", "string_too_long": "length",
		"multiple_of": "multiple", "value_error": "invalid",
	}
	rule, ok := rules[kind]
	if !ok || len(loc) < 2 || len(loc) > 10 || loc[0] != "body" {
		return model.ImageParameterIssue{}, false
	}
	path := ""
	for _, part := range loc[1:] {
		switch v := part.(type) {
		case string:
			if !parameterSegment.MatchString(v) || privateParameter.MatchString(v) {
				return model.ImageParameterIssue{}, false
			}
			if path != "" {
				path += "."
			}
			path += v
		case float64:
			if path == "" || v < 0 || v > 10000 || math.Trunc(v) != v {
				return model.ImageParameterIssue{}, false
			}
			path += fmt.Sprintf("[%d]", int(v))
		default:
			return model.ImageParameterIssue{}, false
		}
	}
	if len(path) > 256 || strings.Contains(path, "__") {
		return model.ImageParameterIssue{}, false
	}
	return model.ImageParameterIssue{Field: path, Rule: rule}, true
}
