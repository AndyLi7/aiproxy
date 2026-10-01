package main

import (
	"encoding/json"
	rv "github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestBriaProductURLAndRawImageReachProviderUnchanged(t *testing.T) {
	data, err := os.ReadFile("../../common/registryvalidation/testdata/bria-overlay.json")
	require.NoError(t, err)
	var fixtures []sample
	require.NoError(t, json.Unmarshal(data, &fixtures))
	require.Len(t, fixtures, 6)
	for _, f := range fixtures {
		result := run(f)
		require.True(t, result.Passed, result.Error)
		require.True(t, result.OutputPassed, result.OutputError)
		var c struct {
			ID        string `json:"entry_id"`
			Providers map[string]struct{ Upstream rv.ProviderSpec }
		}
		require.NoError(t, json.Unmarshal(f.Contract, &c))
		p := c.Providers["fal"].Upstream
		b := rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash}
		normalized, ve := rv.ValidateImage(f.Contract, c.ID, f.Request)
		require.Nil(t, ve)
		mapped, err := rv.MapBoundProviderInput(f.Contract, b, "fal-image", p.Endpoint, "async", normalized)
		require.NoError(t, err)
		var req, native map[string]any
		require.NoError(t, json.Unmarshal(f.Request, &req))
		require.NoError(t, json.Unmarshal(mapped, &native))
		require.Equal(t, req["image"], native["image"])
		for _, format := range []string{"png", "jpeg", "dual"} {
			req["output_format"] = format
			request, _ := json.Marshal(req)
			normalized, ve := rv.ValidateImage(f.Contract, c.ID, request)
			require.Nil(t, ve)
			result, err := rv.MapBoundProviderInput(f.Contract, b, "fal-image", p.Endpoint, "async", normalized)
			require.NoError(t, err)
			var forwarded map[string]any
			require.NoError(t, json.Unmarshal(result, &forwarded))
			require.Equal(t, format, forwarded["output_format"])
			require.Equal(t, false, forwarded["sync"])
		}

		for _, value := range []string{"data:image/png;base64,invalid", "http://127.0.0.1/private", "http://user:pass@example.com/x", "not-base64!", "data:text/html;base64,aGVsbG8="} {
			native["image"] = value
			bad, _ := json.Marshal(native)
			_, err := rv.ResolveImageMetering(f.Contract, b, bad, 1, 1024, true)
			require.Error(t, err, value)
		}
	}
}
