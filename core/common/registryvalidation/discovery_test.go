package registryvalidation

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestDiscoverySharesDimensionConstraintsWithValidation(t *testing.T) {
	contract := []byte(`{"entry_id":"brand/image/text-to-image","validation_version":1,"execution":{"mode":"sync","output":"image"},"input_schema":{"type":"object","required":["prompt","size"],"properties":{"prompt":{"type":"string","minLength":1},"size":{"type":"string","default":"2K"},"n":{"type":"integer","minimum":1,"maximum":4}},"additionalProperties":false,"x-image-constraints":{"version":1,"dimensions":{"presets":["2K","4K"],"minPixels":3686400,"maxPixels":16777216,"minRatio":0.0625,"maxRatio":16}}},"providers":{"secret":{"credential":"must-not-leak"}}}`)
	discovery := DiscoverImage(contract)
	require.NotNil(t, discovery)
	require.Equal(t, float64(4), discovery.Generation["max_images"])
	_, invalid := ValidateImage(contract, "brand/image/text-to-image", []byte(`{"model":"brand/image/text-to-image","prompt":"test","size":"1024x1024"}`))
	require.NotNil(t, invalid)
	constraints := discovery.Generation["size_constraints"].(map[string]any)
	require.Equal(t, "size", constraints["parameter"])
	require.NotNil(t, constraints["schema"])
	for key, value := range invalid.Expected {
		require.Equal(t, value, constraints[key])
	}
	encoded, err := json.Marshal(discovery)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "must-not-leak")
	require.NotContains(t, string(encoded), "providers")
	require.Equal(t, "/v1/images/generations", discovery.API["endpoint"])
}
func TestDiscoveryCountsArePerContract(t *testing.T) {
	for _, tc := range []struct {
		property string
		want     any
	}{
		{`"n":{"type":"integer","const":1,"default":1}`, float64(1)},
		{`"num_images":{"type":"integer","maximum":8}`, float64(8)},
		{`"max_images":{"type":"integer","maximum":5}`, float64(5)},
		{`"prompt":{"type":"string"}`, nil},
	} {
		raw := []byte(`{"validation_version":1,"execution":{"mode":"async","output":"image"},"input_schema":{"type":"object","properties":{` + tc.property + `}}}`)
		result := DiscoverImage(raw)
		require.NotNil(t, result)
		require.Equal(t, tc.want, result.Generation["max_images"])
		require.Equal(t, "/v1/images/tasks", result.API["endpoint"])
	}
}

func TestDiscoveryFixedOutputAndUnifiedDimensions(t *testing.T) {
	d := DiscoverImage([]byte(`{"validation_version":1,"execution":{"mode":"sync","output":"image"},"input_schema":{"type":"object","properties":{"size":{"type":"string","default":"1K"}}},"output_schema":{"type":"object","properties":{"data":{"type":"array","minItems":1,"maxItems":1}}}}`))
	if d == nil || d.Generation["max_images"] != 1 || d.Generation["count_semantics"] != "fixed" || d.Generation["count_parameter"] != nil {
		t.Fatalf("invalid fixed count: %#v", d)
	}
	constraints := d.Generation["size_constraints"].(map[string]any)
	if constraints["parameter"] != "size" || constraints["schema"] == nil {
		t.Fatal("missing canonical size schema")
	}
}
