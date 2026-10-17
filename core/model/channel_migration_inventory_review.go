package model

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"

	"github.com/labring/aiproxy/core/common/nativeresult"
)

// Bound nesting BEFORE any recursive walk. Token parsing is iterative, also
// rejecting duplicate object keys and more than one top-level JSON value.
func inventoryDecode(raw []byte) (any, bool) {
	if len(raw) > MigrationInventoryMaxBytes {
		return nil, false
	}
	type frame struct {
		object bool
		key    bool
		seen   map[string]bool
	}
	stack := []frame{}
	roots := 0
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false
		}
		delim, isDelim := tok.(json.Delim)
		if isDelim && (delim == '}' || delim == ']') {
			if len(stack) == 0 {
				return nil, false
			}
			stack = stack[:len(stack)-1]
			continue
		}
		if len(stack) == 0 {
			roots++
			if roots > 1 {
				return nil, false
			}
		} else {
			f := &stack[len(stack)-1]
			if f.object && f.key {
				k, ok := tok.(string)
				if !ok || f.seen[k] {
					return nil, false
				}
				f.seen[k] = true
				f.key = false
				continue
			}
			if f.object {
				f.key = true
			}
		}
		if isDelim {
			if delim != '{' && delim != '[' {
				return nil, false
			}
			stack = append(stack, frame{delim == '{', delim == '{', map[string]bool{}})
			if len(stack) > 32 {
				return nil, false
			}
		}
	}
	if roots != 1 || len(stack) != 0 {
		return nil, false
	}
	var value any
	d = json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&value) != nil {
		return nil, false
	}
	return value, true
}

var inventoryIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./:@+\-]{0,511}$`)

func inventoryID(value any) bool {
	s, ok := value.(string)
	return ok && (s == "" || inventoryIdentifier.MatchString(s))
}
func inventoryNumbers(value any) bool {
	switch v := value.(type) {
	case nil, bool, json.Number, int, int64, float64:
		return true
	case []any:
		for _, x := range v {
			if !inventoryNumbers(x) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, x := range v {
			if !inventoryNumbers(x) {
				return false
			}
		}
		return true
	}
	return false
}
func inventoryObject(value any, stringsAllowed map[string]bool, children map[string]string) bool {
	obj, ok := value.(map[string]any)
	if !ok {
		return value == nil
	}
	for k, x := range obj {
		if stringsAllowed[k] {
			if !inventoryID(x) {
				return false
			}
			continue
		}
		if child, ok := children[k]; ok {
			if !inventoryTyped(x, child) {
				return false
			}
			continue
		}
		if !inventoryNumbers(x) {
			return false
		}
	}
	return true
}
func inventoryTyped(value any, kind string) bool {
	switch kind {
	case "config":
		return inventoryObject(value, map[string]bool{"public_model": true, "public_capability_model": true, "public_api_id": true, "capability": true}, map[string]string{"upstream_prepayment_v1": "quote", "x_token_platform_native_result_v1": "binding", "support_formats": "ids", "support_voices": "ids", "public_capability_aliases": "ids", "x_token_platform_input_meter_v1": "meter"})
	case "channel":
		obj, ok := value.(map[string]any)
		if !ok {
			return value == nil
		}
		for k, x := range obj {
			switch k {
			case ChannelConfigOutboundPolicy:
				if x != "public_only" {
					return false
				}
			case ChannelConfigProviderBindings:
				bindings, ok := x.(map[string]any)
				if !ok {
					return false
				}
				for model, b := range bindings {
					if !inventoryID(model) || !inventoryObject(b, map[string]bool{"provider": true, "id": true, "revision": true, "contractHash": true}, nil) {
						return false
					}
				}
			default:
				if !inventoryNumbers(x) {
					return false
				}
			}
		}
		return true
	case "quote":
		return inventoryObject(value, map[string]bool{"settlementPolicy": true, "currency": true, "quoteVersion": true}, map[string]string{"routes": "routes"})
	case "routes":
		rows, ok := value.([]any)
		if !ok {
			return false
		}
		for _, r := range rows {
			if !inventoryTyped(r, "route") {
				return false
			}
		}
		return true
	case "route":
		return inventoryObject(value, map[string]bool{"routeId": true, "provider": true, "endpoint": true, "credentialScope": true, "quantityMetric": true, "keyFingerprint": true}, map[string]string{"rule": "rule"})
	case "rule":
		return inventoryObject(value, map[string]bool{"mode": true, "ratio": true}, nil)
	case "binding":
		return inventoryObject(value, map[string]bool{"endpoint": true, "credentialScope": true, "keyFingerprint": true, "deliveryBase": true}, map[string]string{"contract": "contract"})
	case "contract":
		raw, err := json.Marshal(value)
		if err != nil {
			return false
		}
		if _, err = nativeresult.CompileTaskContract(raw); err != nil {
			return false
		}
		obj := value.(map[string]any)
		fixed, ok := obj["fixed_parameters"].(map[string]any)
		if obj["fixed_parameters"] != nil && !ok {
			return false
		}
		for name, v := range fixed {
			if !inventoryFixedControl(name, v, obj["upstream_input_schema"]) {
				return false
			}
		}
		return true // Public frozen schema/default text, not arbitrary provider config.
	case "meter":
		return inventoryObject(value, map[string]bool{"metric": true}, map[string]string{"path": "ids", "routes": "meterRoutes"})
	case "meterRoutes":
		routes, ok := value.(map[string]any)
		if !ok {
			return false
		}
		for id, route := range routes {
			if !inventoryID(id) || !inventoryNumbers(route) {
				return false
			}
		}
		return true
	case "ids":
		items, ok := value.([]any)
		if !ok {
			return false
		}
		for _, x := range items {
			if !inventoryID(x) {
				return false
			}
		}
		return true
	case "plugin":
		obj, ok := value.(map[string]any)
		if !ok {
			return value == nil
		}
		// Actual schemas: relay/plugin/{cache,cachefollow,thinksplit,streamfake}/config.go.
		schemas := map[string]map[string]string{
			"cache":       {"enable": "bool", "ttl": "number", "item_max_size": "number", "add_cache_hit_header": "bool", "cache_hit_header": "id"},
			"cachefollow": {"enable": "bool", "enable_generic_follow": "bool", "followed_channel_ttl_seconds": "number", "recent_channel_update_debounce_seconds": "number"},
			"think-split": {"enable": "bool"}, "stream-fake": {"enable": "bool"},
		}
		for plugin, raw := range obj {
			schema, ok := schemas[plugin]
			if !ok {
				return false
			}
			fields, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			for k, x := range fields {
				typ, ok := schema[k]
				if !ok {
					return false
				}
				switch typ {
				case "bool":
					if _, ok := x.(bool); !ok {
						return false
					}
				case "number":
					n, ok := x.(json.Number)
					if !ok {
						return false
					}
					if _, e := n.Int64(); e != nil {
						return false
					}
				case "id":
					if !inventoryID(x) {
						return false
					}
				}
			}
		}
		return true
	}
	return false
}
func inventoryReviewText(raw any, kind string) bool {
	if raw == nil {
		return true
	}
	s, ok := raw.(string)
	if !ok {
		return false
	}
	if strings.TrimSpace(s) == "" {
		return true
	}
	v, ok := inventoryDecode([]byte(s))
	return ok && inventoryTyped(v, kind)
}

// This is the platform-control set derived by the application's
// registry/import/native-platform-controls.ts, not arbitrary fixed credentials.
func inventoryFixedControl(name string, value any, upstream any) bool {
	switch name {
	case "sync_mode", "sync", "return_byteplus_urls":
		return value == false
	case "enable_safety_checker", "enable_output_safety_checker", "enable_safety_checks", "limit_generations", "limit_outputs", "limit_images":
		return value == true
	case "safety_checker_version":
		return value == "v1"
	case "moderation_level":
		return value == "permissive" || value == "conservative"
	case "safety_tolerance":
		switch v := value.(type) {
		case json.Number:
			return v == "1" || v == "2" || v == "3" || v == "4" || v == "5" || v == "6"
		case string:
			return v == "1" || v == "2" || v == "3" || v == "4" || v == "5" || v == "6"
		}
		return false
	}
	// Dynamic URL-vs-inline delivery switches are reviewed against their actual
	// upstream enum, just as import does. Other named fixed strings are unsupported.
	if value != "url" || !regexp.MustCompile(`(?i)format|output|response|return|result|delivery`).MatchString(name) {
		return false
	}
	root, ok := upstream.(map[string]any)
	if !ok {
		return false
	}
	props, ok := root["properties"].(map[string]any)
	if !ok {
		return false
	}
	field, ok := props[name].(map[string]any)
	if !ok || field["type"] != "string" {
		return false
	}
	choices, ok := field["enum"].([]any)
	if !ok || len(choices) < 2 {
		return false
	}
	hasURL := false
	for _, v := range choices {
		switch v {
		case "url":
			hasURL = true
		case "hex", "base64", "b64", "b64_json", "data_uri", "data_url", "bytes", "binary":
		default:
			return false
		}
	}
	return hasURL
}
