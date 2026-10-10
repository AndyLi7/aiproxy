package registryvalidation

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestNamedImageProjectionRejectsDrift(t *testing.T) {
	raw, err := os.ReadFile("testdata/midas-named-images.json")
	require.NoError(t, err)
	var rows []struct{ Contract json.RawMessage }
	require.NoError(t, json.Unmarshal(raw, &rows))
	var fixture struct {
		Providers map[string]struct{ Upstream ProviderSpec }
	}
	require.NoError(t, json.Unmarshal(rows[0].Contract, &fixture))
	original := fixture.Providers["fal"].Upstream
	require.NoError(t, validateOutputImageProjection(&original))
	for _, mutation := range []func(*ProviderSpec){
		func(p *ProviderSpec) { p.OutputImages.Fields = []string{"depth_map"} },
		func(p *ProviderSpec) { p.OutputImages.Fields = []string{"normal_map", "depth_map"} },
		func(p *ProviderSpec) { p.OutputImages.Version = 3 },
		func(p *ProviderSpec) { p.OutputImages.Mode = "ignore" },
		func(p *ProviderSpec) { p.Output["required"] = []any{"depth_map"} },
		func(p *ProviderSpec) {
			p.Output["properties"].(map[string]any)["depth_map"].(map[string]any)["description"] = "A file."
		},
	} {
		data, _ := json.Marshal(original)
		var changed ProviderSpec
		require.NoError(t, json.Unmarshal(data, &changed))
		mutation(&changed)
		require.Error(t, validateOutputImageProjection(&changed))
	}
}
