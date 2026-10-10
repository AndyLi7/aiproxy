package main

import (
	"context"
	"encoding/json"
	rv "github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"github.com/stretchr/testify/require"
	"net/http"
	"os"
	"testing"
)

func TestActualPatinaMaterialIdentity(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/patina-map-types.json")
	require.NoError(t, err)
	var fixtures []sample
	require.NoError(t, json.Unmarshal(raw, &fixtures))
	for _, f := range fixtures {
		require.True(t, run(f).OutputPassed)
		var c struct {
			Providers map[string]struct{ Upstream rv.ProviderSpec }
		}
		require.NoError(t, json.Unmarshal(f.Contract, &c))
		p := c.Providers["fal"].Upstream
		frozen, err := rv.FreezeProviderBinding(f.Contract, rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash})
		require.NoError(t, err)
		require.True(t, rv.FrozenImageMapTypeEnabled(frozen))
		for _, kind := range []string{"basecolor", "normal", "roughness", "metalness", "height", "unknown", ""} {
			body, _ := json.Marshal(map[string]any{"images": []any{map[string]any{"url": "https://example.com/map.png", "map_type": kind}}, "seed": 0})
			client := fal.Client{BaseURL: "https://offline.invalid", HTTP: &http.Client{Transport: fixtureTransport{body: body}}}
			result, err := client.Poll(context.Background(), p.Endpoint, "audit", frozen)
			require.NoError(t, err)
			if kind == "unknown" || kind == "" {
				require.Equal(t, "failed", result.Status)
				continue
			}
			require.Equal(t, "completed", result.Status)
			require.Equal(t, kind, result.Data[0].MapType)
			legacy, err := client.Poll(context.Background(), p.Endpoint, "audit")
			require.NoError(t, err)
			require.Equal(t, "completed", legacy.Status)
			require.Empty(t, legacy.Data[0].MapType)
		}
	}
}

func TestMaterialSelectionDeterminesMaximumDeliveredImages(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/patina-map-types.json")
	require.NoError(t, err)
	var fixtures []sample
	require.NoError(t, json.Unmarshal(raw, &fixtures))
	f := fixtures[1]
	var contract struct {
		ID        string `json:"entry_id"`
		Providers map[string]struct{ Upstream rv.ProviderSpec }
	}
	require.NoError(t, json.Unmarshal(f.Contract, &contract))
	p := contract.Providers["fal"].Upstream
	binding := rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash}
	for _, count := range []int{-1, 0, 1, 2, 5, 15, 16} {
		var request map[string]any
		require.NoError(t, json.Unmarshal(f.Request, &request))
		size := count
		if size < 0 {
			size = 0
		}
		maps := make([]string, size)
		for i := range maps {
			maps[i] = "normal"
		}
		request["maps"] = maps
		if count == -1 {
			delete(request, "maps")
		}
		data, _ := json.Marshal(request)
		normalized, ve := rv.ValidateImage(f.Contract, contract.ID, data)
		require.Nil(t, ve)
		mapped, err := rv.MapBoundProviderInput(f.Contract, binding, "fal-image", p.Endpoint, "async", normalized)
		require.NoError(t, err)
		evidence, err := rv.ResolveImageMetering(f.Contract, binding, mapped, 1, 1024, true)
		if count == 0 || count == 16 {
			require.Error(t, err)
			continue
		}
		require.NoError(t, err)
		expected := count
		if expected == -1 {
			expected = 5
		}
		require.Equal(t, expected, evidence.MaximumOutputs)
	}
	require.True(t, run(f).OutputPassed, "the default five-map response must pass delivery replay")
	var response struct{ Images []any }
	require.NoError(t, json.Unmarshal(f.Response, &response))
	require.Len(t, response.Images, 5)
}
