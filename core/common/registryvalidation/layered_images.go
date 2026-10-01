package registryvalidation

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
)

// LayeredImageProjection binds ordered gallery images to their layer metadata.
// It is not enabled by a payload field or an endpoint-name heuristic.
type LayeredImageProjection struct {
	Version int    `json:"version"`
	Mode    string `json:"mode"`
	Maximum int    `json:"maximum"`
}

var layerCountDescription = regexp.MustCompile(`^The base image followed by up to (\d+) separated layers, ordered by increasing z_index\.$`)

func layerObject(v any) map[string]any { m, _ := v.(map[string]any); return m }
func layerHas(v any, name string) bool {
	a, _ := v.([]any)
	for _, n := range a {
		if n == name {
			return true
		}
	}
	return false
}
func layerKeys(m map[string]any, names ...string) bool {
	if m == nil {
		return false
	}
	for k := range m {
		found := false
		for _, n := range names {
			if k == n {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func layerNullable(s map[string]any, typ string) map[string]any {
	a, ok := s["anyOf"].([]any)
	if !ok {
		a, _ = s["oneOf"].([]any)
	}
	if len(a) != 2 {
		return nil
	}
	nulls := 0
	var result map[string]any
	for _, v := range a {
		b := layerObject(v)
		if b["type"] == "null" {
			nulls++
		}
		if b["type"] == typ {
			result = b
		}
	}
	if nulls != 1 {
		return nil
	}
	return result
}
func layerTypedImage(s map[string]any) bool {
	if !typedImage(s) || !layerKeys(layerObject(s["properties"]), "url", "width", "height", "content_type", "file_name", "file_size") {
		return false
	}
	props := layerObject(s["properties"])
	required, _ := s["required"].([]any)
	for _, raw := range required {
		name, ok := raw.(string)
		if !ok || props[name] == nil {
			return false
		}
	}
	return true
}
func InferLayeredImageProjection(source map[string]any) (*LayeredImageProjection, error) {
	props := layerObject(source["properties"])
	images, layers := layerObject(props["images"]), layerObject(props["layers"])
	if source["type"] != "object" || !layerHas(source["required"], "images") || !layerHas(source["required"], "layers") || images["type"] != "array" || !layerTypedImage(layerObject(images["items"])) || layers["type"] != "array" || images["description"] != "The same images as `layers`, in the same order, flattened for gallery rendering." {
		return nil, ErrProviderContract
	}
	description, _ := layers["description"].(string)
	match := layerCountDescription.FindStringSubmatch(description)
	if len(match) != 2 {
		return nil, ErrProviderContract
	}
	count, err := strconv.Atoi(match[1])
	if err != nil || count < 1 || count > 1023 {
		return nil, ErrProviderContract
	}
	item := layerObject(layers["items"])
	fields := layerObject(item["properties"])
	z := layerObject(fields["z_index"])
	if item["type"] != "object" || !layerHas(item["required"], "image") || !layerHas(item["required"], "z_index") || !layerKeys(fields, "image", "z_index", "bounding_box", "name", "description") || !layerTypedImage(layerObject(fields["image"])) || z["type"] != "integer" || z["minimum"] != float64(0) || z["maximum"] != float64(count) {
		return nil, ErrProviderContract
	}
	required, _ := item["required"].([]any)
	for _, r := range required {
		n, ok := r.(string)
		if !ok || fields[n] == nil {
			return nil, ErrProviderContract
		}
	}
	if layerNullable(layerObject(fields["name"]), "string") == nil || layerNullable(layerObject(fields["description"]), "string") == nil {
		return nil, ErrProviderContract
	}
	box := layerNullable(layerObject(fields["bounding_box"]), "object")
	coords := layerObject(box["properties"])
	if !layerKeys(coords, "absolute", "normalized") || !layerHas(box["required"], "absolute") || !layerHas(box["required"], "normalized") {
		return nil, ErrProviderContract
	}
	requiredBox, _ := box["required"].([]any)
	for _, raw := range requiredBox {
		name, ok := raw.(string)
		if !ok || coords[name] == nil {
			return nil, ErrProviderContract
		}
	}
	for _, v := range coords {
		f := layerObject(v)
		if f["type"] != "array" || f["minItems"] != float64(4) || f["maxItems"] != float64(4) || layerObject(f["items"])["type"] != "integer" {
			return nil, ErrProviderContract
		}
	}
	return &LayeredImageProjection{Version: 3, Mode: "layers", Maximum: count + 1}, nil
}

// ExtractLayerMetadata preserves explicit nulls while removing source image URLs
// and undeclared payload fields. Main images must be archived through the ordinary path.
func ExtractLayerMetadata(source map[string]any, projection LayeredImageProjection, raw []byte) ([]json.RawMessage, error) {
	expected, err := InferLayeredImageProjection(source)
	if err != nil || *expected != projection || len(raw) > 4*1024*1024 {
		return nil, ErrProviderContract
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil || schemaAccepts(source, body) != nil {
		return nil, ErrProviderContract
	}
	images, _ := body["images"].([]any)
	layers, _ := body["layers"].([]any)
	if len(images) < 1 || len(images) > projection.Maximum || len(images) != len(layers) {
		return nil, ErrProviderContract
	}
	result := make([]json.RawMessage, 0, len(layers))
	previous := -1
	totalMetadataBytes := 0
	for i, v := range layers {
		layer := layerObject(v)
		image := layerObject(layer["image"])
		gallery := layerObject(images[i])
		url, ok := image["url"].(string)
		z, number := layer["z_index"].(float64)
		if !ok || url == "" || gallery["url"] != url || !number || math.Trunc(z) != z || z < 0 || z >= float64(projection.Maximum) || int(z) <= previous || (i == 0 && z != 0) {
			return nil, ErrProviderContract
		}
		previous = int(z)
		safe := map[string]any{"z_index": z}
		for _, key := range []string{"name", "description", "bounding_box"} {
			if value, present := layer[key]; present {
				safe[key] = value
			}
		}
		if box := layerObject(safe["bounding_box"]); box != nil {
			filtered := map[string]any{}
			for _, key := range []string{"absolute", "normalized"} {
				coords, ok := box[key].([]any)
				if !ok || len(coords) != 4 {
					return nil, ErrProviderContract
				}
				for _, value := range coords {
					n, ok := value.(float64)
					if !ok || math.Trunc(n) != n || math.Abs(n) > 9007199254740991 {
						return nil, ErrProviderContract
					}
				}
				filtered[key] = coords
			}
			safe["bounding_box"] = filtered
		}
		encoded, err := json.Marshal(safe)
		totalMetadataBytes += len(encoded)
		if err != nil || totalMetadataBytes > 1024*1024 {
			return nil, ErrProviderContract
		}
		result = append(result, encoded)
	}
	return result, nil
}

// ExtractFrozenLayerMetadata accepts only a source-proven selected binding.
func ExtractFrozenLayerMetadata(raw, result []byte) ([]json.RawMessage, error) {
	if !HasProviderContracts(raw) {
		return nil, nil
	}
	var frozen struct {
		Binding ProviderBinding `json:"selected_provider_binding"`
	}
	if json.Unmarshal(raw, &frozen) != nil {
		return nil, ErrProviderContract
	}
	p, err := bound(raw, frozen.Binding)
	if err != nil {
		return nil, err
	}
	projection := p.Upstream.OutputImages
	if projection == nil || projection.Version != 3 {
		return nil, nil
	}
	return ExtractLayerMetadata(p.Upstream.Output, LayeredImageProjection{Version: 3, Mode: projection.Mode, Maximum: projection.Maximum}, result)
}
