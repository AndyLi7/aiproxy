package main

import (
	"encoding/json"
	rv "github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestRealSourceCardinalityAndOutputOverflow(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/output-cardinality.json")
	require.NoError(t, err)
	var fixtures []sample
	require.NoError(t, json.Unmarshal(raw, &fixtures))
	for _, f := range fixtures {
		t.Run(f.Endpoint+"/"+f.Scenario, func(t *testing.T) {
			actual := run(f)
			require.True(t, actual.Passed, actual.Error)
			require.True(t, actual.OutputPassed, actual.OutputError)
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
			evidence, err := rv.ResolveImageMetering(f.Contract, b, mapped, 1, 1024, true)
			require.NoError(t, err)
			var response map[string]any
			require.NoError(t, json.Unmarshal(f.Response, &response))
			images := response["images"].([]any)
			require.Equal(t, len(images), evidence.MaximumOutputs)
			frozen, err := rv.FreezeProviderBinding(f.Contract, b)
			require.NoError(t, err)
			images = append(images, images[0])
			response["images"] = images
			if _, ok := response["has_nsfw_concepts"]; ok {
				response["has_nsfw_concepts"] = make([]bool, len(images))
			}
			overflow, _ := json.Marshal(response)
			require.Error(t, replayOutput(frozen, p.Endpoint, overflow, true, evidence.MaximumOutputs))
			if p.Endpoint == "fal-ai/qwen-image-layered" || p.Endpoint == "fal-ai/qwen-image-layered/lora" {
				for _, count := range []int{1, 10} {
					var req map[string]any
					require.NoError(t, json.Unmarshal(f.Request, &req))
					req["num_layers"] = count
					data, _ := json.Marshal(req)
					normalized, ve := rv.ValidateImage(f.Contract, c.ID, data)
					require.Nil(t, ve)
					mapped, err := rv.MapBoundProviderInput(f.Contract, b, "fal-image", p.Endpoint, "async", normalized)
					require.NoError(t, err)
					result, err := rv.ResolveImageMetering(f.Contract, b, mapped, 1, 1024, true)
					require.NoError(t, err)
					require.Equal(t, count, result.MaximumOutputs)
				}
			}
		})
	}
}
