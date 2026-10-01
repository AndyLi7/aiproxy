package registryvalidation

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMaskAliasRejectsAmbiguousDestinations(t *testing.T) {
	image := map[string]any{"type": "object", "required": []any{"url"}, "properties": map[string]any{"url": map[string]any{"type": "string"}}}
	props := map[string]any{"image": image, "mask": image}
	p := &ProviderSpec{Output: map[string]any{"type": "object", "required": []any{"image", "mask"}, "properties": props}, OutputArtifacts: &OutputArtifactProjection{Version: 2, Primary: "image", Fields: []string{"mask"}}}
	require.NoError(t, validateOutputArtifactProjection(p))
	for _, value := range []any{nil, image} {
		props["mask_image"] = value
		require.Error(t, validateOutputArtifactProjection(p))
	}
	delete(props, "mask_image")
	p.OutputArtifacts.Version = 1
	require.Error(t, validateOutputArtifactProjection(p))
	p.OutputArtifacts.Version = 2
	p.OutputArtifacts.Fields = []string{"mask_image"}
	require.Error(t, validateOutputArtifactProjection(p))
}

func TestOverlayProjectionRejectsAmbiguousAndUntypedAssets(t *testing.T) {
	image := map[string]any{"type": "object", "required": []any{"url"}, "properties": map[string]any{"url": map[string]any{"type": "string"}}}
	props := map[string]any{"image": image, "transparent_overlay": map[string]any{"anyOf": []any{image, map[string]any{"type": "null"}}}}
	p := &ProviderSpec{Output: map[string]any{"type": "object", "required": []any{"image"}, "properties": props}, OutputArtifacts: &OutputArtifactProjection{Version: 3, Primary: "image", Fields: []string{"transparent_overlay"}}}
	require.NoError(t, validateOutputArtifactProjection(p))
	for _, name := range []string{"mask", "mask_image", "images"} {
		props[name] = nil
		require.Error(t, validateOutputArtifactProjection(p))
		delete(props, name)
	}
	props["transparent_overlay"] = map[string]any{"type": "object"}
	require.Error(t, validateOutputArtifactProjection(p))
}

func TestGenericImageRoleRequiresV11AndSourceEvidence(t *testing.T) {
	field := map[string]any{"type": "string", "description": "Source product image as a public URL or raw base64-encoded image bytes."}
	schema := map[string]any{"type": "object", "properties": map[string]any{"image": field}}
	old, err := nativeImagePaths(schema, 10)
	require.NoError(t, err)
	require.Empty(t, old)
	current, err := nativeImagePaths(schema, 11)
	require.NoError(t, err)
	require.Equal(t, [][]string{{"image"}}, current)
	field["description"] = "A drawing style."
	unrelated, err := nativeImagePaths(schema, 11)
	require.NoError(t, err)
	require.Empty(t, unrelated)
}
