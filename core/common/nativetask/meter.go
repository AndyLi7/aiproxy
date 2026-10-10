package nativetask

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
)

// otherTextLimit is the longest other top-level string a metered request may
// carry (owner decision 2026-10-10, A1.3). A longer one may be billed text the
// meter does not count, so the request holds the published maximum.
const otherTextLimit = 64

var (
	errMeterSchema        = errors.New("frozen input schema declares no top-level properties")
	errMeterInput         = errors.New("upstream input is not a JSON object")
	errMeterFieldMissing  = errors.New("metered field is missing")
	errMeterFieldNotText  = errors.New("metered field is not a top-level string")
	errMeterUndeclaredKey = errors.New("upstream input has a key the frozen schema does not declare")
	errMeterOtherText     = errors.New("upstream input has other long text")
)

// sizeQuoteToInput applies the model's input meter: each metered route holds
// the price of this request's metered text instead of the longest text the
// model accepts. It never fails a request. On any problem it returns the
// published quote byte for byte, with the reason for the operator log.
func sizeQuoteToInput(encoded string, quote *model.ImagePrepaymentQuote, rawMeter json.RawMessage, frozen nativeresult.TaskContract, input json.RawMessage) (string, *model.ImagePrepaymentQuote, error) {
	meter, err := model.ParseInputMeter(rawMeter, quote)
	if err != nil {
		return encoded, quote, err
	}
	quantity, err := meteredCharacters(frozen, input, meter.Path[0])
	if err != nil {
		return encoded, quote, err
	}
	sized, sizedJSON, err := meter.Apply(encoded, quote, quantity)
	if err != nil {
		return encoded, quote, err
	}
	return sizedJSON, sized, nil
}

// meteredCharacters counts the Unicode code points of the metered top-level
// string in the body sent upstream, after JSON unescaping: the count the
// maxLength validation uses (Python len()). It refuses when other input could
// also be billed text: a top-level key the frozen schema does not declare,
// another top-level string longer than otherTextLimit whose schema is not an
// enum or const, or another top-level array or object whose strings together
// are longer than otherTextLimit.
func meteredCharacters(frozen nativeresult.TaskContract, input json.RawMessage, field string) (int64, error) {
	// The upstream schema validates the body sent upstream, platform controls
	// included; without platform controls it is the public input schema.
	schema := frozen.UpstreamInputSchema
	if len(schema) == 0 {
		schema = frozen.InputSchema
	}
	// Imported contracts keep the request schema as a local reference into
	// their own components; follow it as the schema validator does.
	root := resolveSchema(schema, schema, 0)
	var properties map[string]json.RawMessage
	if root == nil || json.Unmarshal(root["properties"], &properties) != nil || properties == nil {
		return 0, errMeterSchema
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(input, &fields) != nil || fields == nil {
		return 0, errMeterInput
	}
	raw, present := fields[field]
	if !present {
		return 0, errMeterFieldMissing
	}
	text, isText := jsonString(raw)
	if !isText {
		return 0, errMeterFieldNotText
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		declared, ok := properties[key]
		if !ok {
			return 0, errMeterUndeclaredKey
		}
		if key == field {
			continue
		}
		if other, isText := jsonString(fields[key]); isText && utf8.RuneCountInString(other) > otherTextLimit && !enumerated(resolveSchema(declared, schema, 0)) {
			return 0, errMeterOtherText
		}
		// Text inside a list or object (a dialogue turn, a context block) may
		// be billed too; its schema is not consulted, so short settings such as
		// a voice ID still meter while long nested text holds the maximum.
		if nestedTextLength(fields[key], otherTextLimit) > otherTextLimit {
			return 0, errMeterOtherText
		}
	}
	return int64(utf8.RuneCountInString(text)), nil
}

// nestedTextLength counts the code points of every string inside a JSON array
// or object, stopping once the total passes limit. Other values count 0.
func nestedTextLength(raw json.RawMessage, limit int) int {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || (trimmed[0] != '[' && trimmed[0] != '{') {
		return 0
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		// The body already passed schema validation; an undecodable value is
		// treated as possible text so the request holds the maximum.
		return limit + 1
	}
	total := 0
	var walk func(any) bool
	walk = func(node any) bool {
		switch typed := node.(type) {
		case string:
			total += utf8.RuneCountInString(typed)
		case []any:
			for _, item := range typed {
				if !walk(item) {
					return false
				}
			}
		case map[string]any:
			for _, item := range typed {
				if !walk(item) {
					return false
				}
			}
		}
		return total <= limit
	}
	walk(value)
	return total
}

// jsonString decodes a JSON string value; escapes such as 😀 become
// the one code point they encode.
func jsonString(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var text string
	if json.Unmarshal(trimmed, &text) != nil {
		return "", false
	}
	return text, true
}

// enumerated reports a resolved property schema that fixes its values with
// enum or const.
func enumerated(keywords map[string]json.RawMessage) bool {
	_, isEnum := keywords["enum"]
	_, isConst := keywords["const"]
	return isEnum || isConst
}

// schemaAnnotations may stand next to a $ref or a single allOf branch without
// changing what the schema accepts. Same set as the application's
// native-usage-estimate resolveSchema, which picks the metered field.
var schemaAnnotations = map[string]bool{
	"title": true, "description": true, "examples": true, "default": true, "deprecated": true, "readOnly": true,
	"writeOnly": true, "components": true, "$defs": true, "definitions": true, "$schema": true, "nullable": true,
}

const maxSchemaDepth = 32

// onlyAnnotations reports that every keyword other than keep is an annotation
// or a vendor extension (x-...).
func onlyAnnotations(node map[string]json.RawMessage, keep string) bool {
	for key := range node {
		if key != keep && !schemaAnnotations[key] && !strings.HasPrefix(key, "x-") {
			return false
		}
	}
	return true
}

// resolveSchema follows local references (#/...) into document and
// single-branch allOf wrappers whose siblings are annotations only, as the
// application does. Anything else, such as a remote or conditional schema,
// is returned as is or unresolved (nil), which holds the published maximum.
func resolveSchema(node, document json.RawMessage, depth int) map[string]json.RawMessage {
	var keywords map[string]json.RawMessage
	if depth > maxSchemaDepth || json.Unmarshal(node, &keywords) != nil || keywords == nil {
		return nil
	}
	if rawRef, isRef := keywords["$ref"]; isRef {
		var ref string
		if json.Unmarshal(rawRef, &ref) != nil || !strings.HasPrefix(ref, "#/") || !onlyAnnotations(keywords, "$ref") {
			return nil
		}
		target := document
		for _, part := range strings.Split(ref[2:], "/") {
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			var object map[string]json.RawMessage
			if json.Unmarshal(target, &object) != nil || object == nil {
				return nil
			}
			next, exists := object[part]
			if !exists {
				return nil
			}
			target = next
		}
		return resolveSchema(target, document, depth+1)
	}
	if rawAll, isAll := keywords["allOf"]; isAll && onlyAnnotations(keywords, "allOf") {
		var branches []json.RawMessage
		if json.Unmarshal(rawAll, &branches) == nil && len(branches) == 1 {
			return resolveSchema(branches[0], document, depth+1)
		}
	}
	return keywords
}
