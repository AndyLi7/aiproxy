package registryvalidation

import "encoding/json"

// ImageDiscovery exposes only the public contract. Provider identities, bindings,
// transport constants and upstream schemas never leave the gateway through this API.
type ImageDiscovery struct {
	InputSchema map[string]any `json:"input_schema"`
	Generation  map[string]any `json:"generation"`
	API         map[string]any `json:"api"`
}

func DiscoverImage(raw []byte) *ImageDiscovery {
	var c struct {
		Version      int            `json:"validation_version"`
		Schema       map[string]any `json:"input_schema"`
		OutputSchema map[string]any `json:"output_schema"`
		Execution    struct {
			Mode   string `json:"mode"`
			Output string `json:"output"`
		} `json:"execution"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Version != 1 || c.Schema == nil || c.Execution.Output != "image" || (c.Execution.Mode != "sync" && c.Execution.Mode != "async") {
		return nil
	}
	rules, ok := parseImageRules(c.Schema)
	if !ok {
		return nil
	}
	properties, _ := c.Schema["properties"].(map[string]any)
	defaults := map[string]any{}
	for k, v := range properties {
		if p, ok := v.(map[string]any); ok {
			if d, exists := p["default"]; exists {
				defaults[k] = d
			}
		}
	}
	generation := map[string]any{"mode": c.Execution.Mode, "defaults": defaults, "max_images": nil, "default_size_policy": "model_selected"}
	countName := "n"
	count, _ := properties["n"].(map[string]any)
	if count == nil {
		count, _ = properties["num_images"].(map[string]any)
		countName = "num_images"
	}
	if count == nil {
		count, _ = properties["max_images"].(map[string]any)
		countName = "max_images"
	}
	if count != nil {
		generation["count_parameter"] = countName
		if v, ok := count["const"]; ok {
			generation["max_images"] = v
		} else if v, ok := count["maximum"]; ok {
			generation["max_images"] = v
		} else if values, ok := count["enum"].([]any); ok && len(values) > 0 {
			maximum := float64(0)
			valid := true
			for _, v := range values {
				n, ok := v.(float64)
				if !ok {
					valid = false
					break
				}
				if n > maximum {
					maximum = n
				}
			}
			if valid {
				generation["max_images"] = maximum
			}
		}
	}
	if group, ok := properties["sequential_image_generation_options"].(map[string]any); ok {
		gp, _ := group["properties"].(map[string]any)
		if limit, ok := gp["max_images"].(map[string]any); ok && count == nil {
			generation["max_images"] = limit["maximum"]
			generation["count_parameter"] = "sequential_image_generation_options.max_images"
			generation["count_requires"] = map[string]any{"sequential_image_generation": "auto"}
		}
		generation["count_semantics"] = "maximum"
	} else {
		generation["count_semantics"] = "model_defined"
		if properties["max_images"] != nil {
			generation["count_semantics"] = "maximum"
		}
	}
	if count == nil && generation["max_images"] == nil {
		if props, ok := c.OutputSchema["properties"].(map[string]any); ok {
			if data, ok := props["data"].(map[string]any); ok && data["minItems"] == float64(1) && data["maxItems"] == float64(1) {
				generation["max_images"] = 1
				generation["count_parameter"] = nil
				generation["count_semantics"] = "fixed"
			}
		}
	}
	if _, ok := defaults["size"]; ok {
		generation["default_size_policy"] = "declared_default"
	}
	if _, ok := defaults["image_size"]; ok {
		generation["default_size_policy"] = "declared_default"
	}
	if properties["size"] == nil && properties["image_size"] != nil {
		generation["parameter_aliases"] = map[string]any{"size": map[string]any{"target": "image_size", "custom_format": "WIDTHxHEIGHT", "schema": properties["image_size"]}}
	}
	if size := properties["image_size"]; size != nil {
		generation["size_constraints"] = map[string]any{"parameter": "image_size", "schema": size}
	} else if size := properties["size"]; size != nil {
		generation["size_constraints"] = map[string]any{"parameter": "size", "schema": size}
	}
	if rules.CombinedImages != nil {
		generation["max_combined_images"] = rules.CombinedImages.Maximum
	}
	if rules.Dimensions != nil {
		constraints := dimensionExpectations(rules)
		parameter := "size"
		if properties["image_size"] != nil {
			parameter = "image_size"
		}
		constraints["parameter"] = parameter
		constraints["schema"] = properties[parameter]
		generation["size_constraints"] = constraints
	}
	api := map[string]any{"method": "POST", "endpoint": "/v1/images/generations"}
	if c.Execution.Mode == "async" {
		api["endpoint"] = "/v1/images/tasks"
		api["status_endpoint"] = "/v1/images/tasks/{id}"
	}
	return &ImageDiscovery{InputSchema: c.Schema, Generation: generation, API: api}
}

func dimensionExpectations(rules imageRules) map[string]any {
	d := rules.Dimensions
	return map[string]any{"presets": d.Presets, "custom_format": "WIDTHxHEIGHT", "min_pixels": d.MinPixels, "max_pixels": d.MaxPixels, "min_aspect_ratio": d.MinRatio, "max_aspect_ratio": d.MaxRatio}
}
