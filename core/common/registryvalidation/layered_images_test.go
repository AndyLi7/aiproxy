package registryvalidation

import (
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"os"
	"strings"
	"testing"
)

func layerFixture(t *testing.T) map[string]any {
	raw, e := os.ReadFile("testdata/seedream-layer-output.json")
	require.NoError(t, e)
	var s map[string]any
	require.NoError(t, json.Unmarshal(raw, &s))
	return s
}
func layerResponse(n int) map[string]any {
	images := []any{}
	layers := []any{}
	for i := 0; i < n; i++ {
		image := map[string]any{"url": fmt.Sprintf("https://example.com/%d.png", i)}
		images = append(images, image)
		layers = append(layers, map[string]any{"image": image, "z_index": i, "name": nil, "description": "layer", "bounding_box": map[string]any{"absolute": []int{0, 0, 100, 100}, "normalized": []int{0, 0, 1000, 1000}}, "private_debug": "omit"})
	}
	return map[string]any{"images": images, "layers": layers}
}
func TestLayerSourceAndCompleteProjection(t *testing.T) {
	s := layerFixture(t)
	p, e := InferLayeredImageProjection(s)
	require.NoError(t, e)
	require.Equal(t, 17, p.Maximum)
	for _, n := range []int{1, 16, 17} {
		body, _ := json.Marshal(layerResponse(n))
		out, e := ExtractLayerMetadata(s, *p, body)
		require.NoError(t, e)
		require.Len(t, out, n)
		require.NotContains(t, string(out[0]), "https:")
		require.NotContains(t, string(out[0]), "private_debug")
		require.Contains(t, string(out[0]), `"name":null`)
	}
}
func TestLayerAssociationAndCapacityFailures(t *testing.T) {
	s := layerFixture(t)
	p, e := InferLayeredImageProjection(s)
	require.NoError(t, e)
	for _, mode := range []string{"mismatch", "order", "base", "count", "overflow"} {
		body := layerResponse(2)
		layers := body["layers"].([]any)
		switch mode {
		case "mismatch":
			layerObject(layers[1])["image"] = map[string]any{"url": "https://example.com/wrong.png"}
		case "order":
			layerObject(layers[1])["z_index"] = 0
		case "base":
			layerObject(layers[0])["z_index"] = 1
		case "count":
			body["layers"] = layers[:1]
		case "overflow":
			body = layerResponse(18)
		}
		raw, _ := json.Marshal(body)
		_, e := ExtractLayerMetadata(s, *p, raw)
		require.Error(t, e, mode)
	}
	p.Maximum = 16
	raw, _ := json.Marshal(layerResponse(1))
	_, e = ExtractLayerMetadata(s, *p, raw)
	require.Error(t, e)
}

func TestLayerSourceCannotSilentlyDropNewAssetOrMetadataFields(t *testing.T) {
	for _, mode := range []string{"asset", "required", "bounds", "alias"} {
		s := layerFixture(t)
		p := layerObject(s["properties"])
		layers := layerObject(layerObject(p["layers"])["items"])
		fields := layerObject(layers["properties"])
		switch mode {
		case "asset":
			fields["download_url"] = map[string]any{"type": "string"}
		case "required":
			layerObject(fields["image"])["required"] = []any{"url", "private_field"}
		case "bounds":
			layerObject(fields["z_index"])["maximum"] = float64(18)
		case "alias":
			layerObject(p["images"])["description"] = "Other images."
		}
		_, err := InferLayeredImageProjection(s)
		require.Error(t, err, mode)
	}
}

func TestLayerMetadataBudgetAndSparseOrderedIndices(t *testing.T) {
	s := layerFixture(t)
	p, e := InferLayeredImageProjection(s)
	require.NoError(t, e)
	body := layerResponse(2)
	layers := body["layers"].([]any)
	layerObject(layers[1])["z_index"] = 4
	raw, _ := json.Marshal(body)
	_, e = ExtractLayerMetadata(s, *p, raw)
	require.NoError(t, e)
	for _, l := range layers {
		layerObject(l)["description"] = strings.Repeat("x", 600*1024)
	}
	raw, _ = json.Marshal(body)
	_, e = ExtractLayerMetadata(s, *p, raw)
	require.Error(t, e)
}

func TestBoundedLayerCardinalityRequiresSourceProof(t *testing.T) {
	source := layerFixture(t)
	raw := json.RawMessage(`{"version":2,"mode":"bounded","maximum":17}`)
	count, err := resolveOutputCardinality(raw, nil, nil, 1024, source)
	require.NoError(t, err)
	require.Equal(t, 17, count)
	_, err = resolveOutputCardinality(raw, nil, nil, 1024)
	require.Error(t, err)
	_, err = resolveOutputCardinality(raw, nil, nil, 16, source)
	require.Error(t, err)
	for _, bad := range []string{`{"version":2,"mode":"bounded","maximum":16}`, `{"version":2,"mode":"fixed","maximum":17}`, `{"version":2,"mode":"bounded","maximum":17,"parameter":"n"}`} {
		_, err = resolveOutputCardinality(json.RawMessage(bad), nil, nil, 1024, source)
		require.Error(t, err)
	}
	source["properties"].(map[string]any)["images"].(map[string]any)["description"] = "unproven order"
	_, err = resolveOutputCardinality(raw, nil, nil, 1024, source)
	require.Error(t, err)
}
