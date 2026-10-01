package registryvalidation

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCombinedOutputContractRequiresTypedImagesAndExplicitVersion(t *testing.T) {
	image := map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}, "required": []any{"url"}}
	properties := map[string]any{"image": image, "images": map[string]any{"type": "array", "items": image}}
	spec := &ProviderSpec{Output: map[string]any{"type": "object", "properties": properties, "required": []any{"image"}}, OutputImages: &OutputImageProjection{Version: 1, Mode: "merge-identical"}}
	require.NoError(t, validateOutputImageProjection(spec))
	spec.OutputImages.Version = 2
	require.Error(t, validateOutputImageProjection(spec))
	spec.OutputImages.Version = 1
	spec.OutputImages.Mode = "ignore-array"
	require.Error(t, validateOutputImageProjection(spec))
	spec.OutputImages.Mode = "merge-identical"
	spec.Output["required"] = []any{}
	require.Error(t, validateOutputImageProjection(spec))
	spec.Output["required"] = []any{"image"}
	properties["images"] = map[string]any{"type": "array", "items": map[string]any{"type": "object"}}
	require.Error(t, validateOutputImageProjection(spec))
	properties["images"] = map[string]any{"type": "array", "items": image}
	properties["image"] = map[string]any{"anyOf": []any{image, map[string]any{"type": "null"}}}
	require.NoError(t, validateOutputImageProjection(spec))
}
