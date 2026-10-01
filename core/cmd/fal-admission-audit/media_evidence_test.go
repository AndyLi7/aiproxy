package main

import (
	"encoding/json"
	rv "github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestRealRasterMediaEvidenceContracts(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/provider-media-evidence.json")
	require.NoError(t, err)
	var cases []sample
	require.NoError(t, json.Unmarshal(raw, &cases))
	require.Len(t, cases, 3)
	for _, f := range cases {
		t.Run(f.Endpoint, func(t *testing.T) {
			r := run(f)
			require.True(t, r.Passed)
			require.True(t, r.OutputPassed)
			var c struct {
				Providers map[string]struct{ Upstream rv.ProviderSpec }
			}
			require.NoError(t, json.Unmarshal(f.Contract, &c))
			p := c.Providers["fal"].Upstream
			b := rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash}
			var request map[string]any
			require.NoError(t, json.Unmarshal(f.Request, &request))
			key, value := "output_format", "png"
			if f.Endpoint == "bria/fibo-edit/restyle" {
				key, value = "style", "Vector Art"
			}
			request[key] = value
			data, _ := json.Marshal(request)
			normalized, ve := rv.ValidateImage(f.Contract, request["model"].(string), data)
			require.Nil(t, ve)
			mapped, err := rv.MapBoundProviderInput(f.Contract, b, "fal-image", p.Endpoint, "async", normalized)
			require.NoError(t, err)
			var native map[string]any
			require.NoError(t, json.Unmarshal(mapped, &native))
			require.Equal(t, value, native[key])
			request[key] = "svg"
			data, _ = json.Marshal(request)
			_, ve = rv.ValidateImage(f.Contract, request["model"].(string), data)
			require.NotNil(t, ve)
		})
	}
}
