package registryvalidation

import (
	"encoding/json"
	"net"
	"net/url"
	"reflect"
	"strings"
)

var nativeImageNames = map[string]bool{"image_url": true, "image_urls": true, "reference_image_url": true, "reference_image_urls": true, "input_image_url": true, "input_image_urls": true, "mask_url": true, "mask_image_url": true, "reference_mask_urls": true, "model_image": true, "garment_image": true, "frontal_image_url": true, "control_image_url": true, "person_image_url": true, "garment_image_urls": true}

var extendedNativeImageNames = map[string]bool{"image_source": true, "fill_image_url": true, "control_lora_image_url": true, "garment_image_url": true, "human_image_url": true, "inpaint_image_url": true, "product_image_url": true, "product_image_urls": true, "ip_adapter_mask_url": true, "ip_adapter_image_url": true, "ic_light_image_url": true, "ic_light_model_background_image_url": true, "normal_image_url": true, "segmentation_image_url": true, "depth_image_url": true, "canny_image_url": true, "teed_image_url": true, "openpose_image_url": true, "ref_image_url": true, "end_image_url": true, "start_image_url": true, "uov_image_url": true, "style_image_url": true, "dissolve_image_url": true, "structure_image_url": true, "change_map_image_url": true, "city_image_url": true, "style_reference_image_url": true, "clothing_image_url": true, "face_image_url": true, "pose_image_url": true, "composition_image_url": true, "identity_image_url": true, "initial_image_url": true, "content_image_url": true}

// Paths are checked against the bound schema; payload keys never declare media roles.
func nativeImagePaths(schema map[string]any, versions ...int) ([][]string, error) {
	paths := [][]string{}
	seen := map[string]bool{}
	nodes := 0
	var walk func(any, []string, string, bool, int) error
	walk = func(raw any, path []string, name string, media bool, depth int) error {
		nodes++
		if nodes > 10000 || depth > 32 {
			return ErrProviderContract
		}
		node, ok := raw.(map[string]any)
		if !ok {
			return ErrProviderContract
		}
		description, _ := node["description"].(string)
		genericImage := len(versions) > 0 && (versions[0] == 11 || versions[0] == 12) && name == "image" && strings.Contains(strings.ToLower(description), "public url or raw base64-encoded image bytes")
		image := media || genericImage || nativeImageNames[name] || (len(versions) > 0 && (versions[0] == 7 || versions[0] == 8 || versions[0] == 9 || (versions[0] == 10 || (versions[0] == 11 || versions[0] == 12))) && extendedNativeImageNames[name])
		if node["type"] == "string" && image {
			encoded, _ := json.Marshal(path)
			key := string(encoded)
			if !seen[key] {
				seen[key] = true
				paths = append(paths, append([]string{}, path...))
			}
			return nil
		}
		union := false
		for _, k := range []string{"anyOf", "oneOf"} {
			if branches, ok := node[k].([]any); ok {
				union = true
				for _, branch := range branches {
					if err := walk(branch, path, name, image, depth+1); err != nil {
						return err
					}
				}
			}
		}
		if union {
			return nil
		}
		if node["type"] == "array" {
			return walk(node["items"], append(append([]string{}, path...), "*"), name, image, depth+1)
		}
		if node["type"] == "object" || (node["type"] == nil && node["properties"] != nil) {
			if props, ok := node["properties"].(map[string]any); ok {
				for k, v := range props {
					if err := walk(v, append(append([]string{}, path...), k), k, false, depth+1); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if image && node["type"] != "null" {
			return ErrProviderContract
		}
		return nil
	}
	if err := walk(schema, nil, "", false, 0); err != nil {
		return nil, err
	}
	return paths, nil
}
func resolveNativeImages(paths [][]string, schema map[string]any, body map[string]any, versions ...int) (int64, error) {
	version := 6
	if len(versions) > 0 {
		version = versions[0]
	}
	return resolveNativeImagesWithInlinePolicy(paths, schema, body, version, nil)
}

func resolveNativeImagesWithInlinePolicy(paths [][]string, schema map[string]any, body map[string]any, version int, inline []NativeInlineImageBinding) (int64, error) {
	versions := []int{version}
	if version == 12 {
		props, _ := schema["properties"].(map[string]any)
		matched := false
		for _, name := range []string{"model_image", "garment_image"} {
			field, _ := props[name].(map[string]any)
			label := "model"
			if name == "garment_image" {
				label = "garment"
			}
			if field["type"] != "string" || field["format"] != nil || field["description"] != "URL or base64 of the "+label+" image" {
				continue
			}
			found := false
			for _, binding := range inline {
				if len(binding.Path) == 1 && binding.Path[0] == name {
					if binding.Encoding != "data-uri" || !binding.AllowURL || binding.MediaTypeValidation != "upstream" || len(binding.MediaTypes) != 0 || binding.MaxDecodedBytes != 50*1024*1024 {
						return 0, ErrProviderContract
					}
					found = true
				}
			}
			if !found {
				return 0, ErrProviderContract
			}
			matched = true
		}
		if !matched {
			return 0, ErrProviderContract
		}
	}

	if ((version == 9 || (version == 10 || (version == 11 || version == 12))) && len(inline) == 0) || (version != 9 && (version != 10 && (version != 11 && version != 12)) && len(inline) != 0) {
		return 0, ErrProviderContract
	}
	policies := map[string]NativeInlineImageBinding{}
	for _, binding := range inline {
		if binding.MediaTypeValidation != "" && (version != 10 && (version != 11 && version != 12)) {
			return 0, ErrProviderContract
		}
		if validateInlineImagePolicy(binding.NativeInlineImagePolicy) != nil {
			return 0, ErrProviderContract
		}
		known := false
		for _, path := range paths {
			if reflect.DeepEqual(path, binding.Path) {
				known = true
			}
		}
		encoded, _ := json.Marshal(binding.Path)
		if _, duplicate := policies[string(encoded)]; !known || duplicate {
			return 0, ErrProviderContract
		}
		policies[string(encoded)] = binding
	}
	expected, err := nativeImagePaths(schema, versions...)
	if err != nil || len(paths) != len(expected) {
		return 0, ErrProviderContract
	}
	for _, p := range paths {
		found := false
		for _, e := range expected {
			if reflect.DeepEqual(p, e) {
				found = true
				break
			}
		}
		if !found {
			return 0, ErrProviderContract
		}
	}
	seen := map[string]bool{}
	var count int64
	var currentPolicy *NativeInlineImageBinding
	var visit func(any, []string) error
	visit = func(value any, path []string) error {
		if value == nil {
			return nil
		}
		if len(path) == 0 {
			text, ok := value.(string)
			if !ok {
				return ErrProviderContract
			}
			if currentPolicy != nil {
				if ValidateNativeInlineImage(text, currentPolicy.NativeInlineImagePolicy) == nil {
					count++
					return nil
				}
				if !currentPolicy.AllowURL {
					return ErrProviderContract
				}
			}
			u, err := url.Parse(text)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
				return ErrProviderContract
			}
			host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
			ip := net.ParseIP(host)
			if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || (ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast())) {
				return ErrProviderContract
			}
			count++
			return nil
		}
		if path[0] == "*" {
			values, ok := value.([]any)
			if !ok {
				return ErrProviderContract
			}
			for _, v := range values {
				if err := visit(v, path[1:]); err != nil {
					return err
				}
			}
			return nil
		}
		obj, ok := value.(map[string]any)
		if !ok {
			return ErrProviderContract
		}
		return visit(obj[path[0]], path[1:])
	}
	for _, path := range paths {
		encoded, _ := json.Marshal(path)
		key := string(encoded)
		currentPolicy = nil
		if policy, present := policies[key]; present {
			currentPolicy = &policy
		}
		if seen[key] {
			return 0, ErrProviderContract
		}
		seen[key] = true
		// v8 retains an explicit optional empty default as "no image". Do not
		// extend this to required fields, nested leaves, or array members.
		if len(versions) > 0 && (versions[0] == 8 || versions[0] == 9 || (versions[0] == 10 || (versions[0] == 11 || versions[0] == 12))) && len(path) == 1 && body[path[0]] == "" && currentPolicy == nil {
			props, _ := schema["properties"].(map[string]any)
			field, _ := props[path[0]].(map[string]any)
			required := false
			for _, name := range arrayValues(schema["required"]) {
				if name == path[0] {
					required = true
				}
			}
			if !required && declaresEmptyImage(field) && schemaAccepts(field, "") == nil {
				continue
			}
		}
		if err := visit(body, path); err != nil {
			return 0, err
		}
	}
	return count, nil
}

func arrayValues(v any) []any { values, _ := v.([]any); return values }

// The compiler encodes the empty sentinel explicitly; upstream schemas do not
// apply defaults, so the declaration is the URI-or-empty union, not a default.
func declaresEmptyImage(field map[string]any) bool {
	branches, ok := field["anyOf"].([]any)
	if !ok || len(branches) != 2 {
		return false
	}
	uri, a := branches[0].(map[string]any)
	empty, b := branches[1].(map[string]any)
	return a && b && uri["type"] == "string" && uri["format"] == "uri" &&
		empty["type"] == "string" && empty["const"] == "" && len(empty) == 2
}
