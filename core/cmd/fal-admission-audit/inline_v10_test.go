package main

import (
	"encoding/json"
	rv "github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestCCSRURLAndDataURIReachProviderUnchanged(t *testing.T) {
	data, err := os.ReadFile("../../common/registryvalidation/testdata/ccsr-inline-v10.json")
	require.NoError(t, err)
	var fixtures []sample
	require.NoError(t, json.Unmarshal(data, &fixtures))
	require.Len(t, fixtures, 4)
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
		require.Equal(t, req["image_url"], native["image_url"])
		for _, value := range []string{"data:image/png;base64,invalid", "http://127.0.0.1/private", "http://user:pass@example.com/x", "aGVsbG8=", "data:text/html;base64,aGVsbG8="} {
			native["image_url"] = value
			bad, _ := json.Marshal(native)
			_, err := rv.ResolveImageMetering(f.Contract, b, bad, 1, 1024, true)
			require.Error(t, err, value)
		}
	}
}
