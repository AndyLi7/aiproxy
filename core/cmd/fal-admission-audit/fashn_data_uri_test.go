package main

import (
	"encoding/json"
	rv "github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestFashnDataURIPreservesBytesAndSafetyConstraints(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/fashn-data-uri.json")
	require.NoError(t, err)
	var rows []sample
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.Len(t, rows, 20)
	for _, row := range rows {
		t.Run(row.Endpoint+"/"+row.Scenario, func(t *testing.T) {
			checked := run(row)
			require.True(t, checked.Passed, checked.Error)
			require.True(t, checked.OutputPassed, checked.OutputError)
			var contract struct {
				ID        string `json:"entry_id"`
				Providers map[string]struct{ Upstream rv.ProviderSpec }
			}
			require.NoError(t, json.Unmarshal(row.Contract, &contract))
			p := contract.Providers["fal"].Upstream
			binding := rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash}
			normalized, ve := rv.ValidateImage(row.Contract, contract.ID, row.Request)
			require.Nil(t, ve)
			mapped, err := rv.MapBoundProviderInput(row.Contract, binding, "fal-image", p.Endpoint, "async", normalized)
			require.NoError(t, err)
			var original, actual map[string]any
			require.NoError(t, json.Unmarshal(row.Request, &original))
			require.NoError(t, json.Unmarshal(mapped, &actual))
			for _, name := range []string{"model_image", "garment_image"} {
				require.Equal(t, original[name], actual[name])
			}

			for _, count := range []int{1, 2, 4, 5} {
				modified := map[string]any{}
				require.NoError(t, json.Unmarshal(row.Request, &modified))
				modified["n"] = count
				data, _ := json.Marshal(modified)
				normalized, ve := rv.ValidateImage(row.Contract, contract.ID, data)
				if count == 5 {
					require.NotNil(t, ve)
					continue
				}
				require.Nil(t, ve)
				mapped, err := rv.MapBoundProviderInput(row.Contract, binding, "fal-image", p.Endpoint, "async", normalized)
				require.NoError(t, err)
				var native map[string]any
				require.NoError(t, json.Unmarshal(mapped, &native))
				require.EqualValues(t, count, native["num_samples"])
				evidence, err := rv.ResolveImageMetering(row.Contract, binding, mapped, count, 1024, true)
				require.NoError(t, err)
				require.Equal(t, count, evidence.MaximumOutputs)
			}
			for _, bad := range []string{"none", "unknown"} {
				modified := map[string]any{}
				require.NoError(t, json.Unmarshal(row.Request, &modified))
				modified["moderation_level"] = bad
				data, _ := json.Marshal(modified)
				_, ve = rv.ValidateImage(row.Contract, contract.ID, data)
				require.NotNil(t, ve)
			}
			for _, bad := range []string{"aGVsbG8=", "data:text/html;base64,aGVsbG8=", "http://127.0.0.1/image.png"} {
				modified := map[string]any{}
				require.NoError(t, json.Unmarshal(mapped, &modified))
				modified["model_image"] = bad
				data, _ := json.Marshal(modified)
				n := 1
				if value, ok := original["n"].(float64); ok {
					n = int(value)
				}
				_, err = rv.ResolveImageMetering(row.Contract, binding, data, n, 1024, true)
				require.Error(t, err)
			}
		})
	}
}
