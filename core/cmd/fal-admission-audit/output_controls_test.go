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

func TestLCMInternalOutputControls(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/lcm-output-controls.json")
	require.NoError(t, err)
	var fixtures []sample
	require.NoError(t, json.Unmarshal(raw, &fixtures))
	require.Len(t, fixtures, 2)
	for _, f := range fixtures {
		r := run(f)
		require.True(t, r.Passed, r.Error)
		require.True(t, r.OutputPassed, r.OutputError)
		var contract struct {
			Providers map[string]struct{ Upstream rv.ProviderSpec }
		}
		require.NoError(t, json.Unmarshal(f.Contract, &contract))
		p := contract.Providers["fal"].Upstream
		frozen, err := rv.FreezeProviderBinding(f.Contract, rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash})
		require.NoError(t, err)
		for _, scenario := range []string{"safe", "flagged", "missing", "misaligned", "wrong-type"} {
			t.Run(f.Scenario+"/"+scenario, func(t *testing.T) {
				var body map[string]any
				require.NoError(t, json.Unmarshal(f.Response, &body))
				count := len(body["images"].([]any))
				flags := make([]bool, count)
				body["nsfw_content_detected"] = flags
				body["request_id"] = "private-upstream-id"
				switch scenario {
				case "flagged":
					flags[0] = true
				case "missing":
					delete(body, "nsfw_content_detected")
				case "misaligned":
					body["nsfw_content_detected"] = append(flags, false)
				case "wrong-type":
					body["nsfw_content_detected"] = []string{"false"}
				}
				data, _ := json.Marshal(body)
				client := fal.Client{BaseURL: "https://offline.invalid", HTTP: &http.Client{Transport: fixtureTransport{body: data}}}
				result, err := client.Poll(context.Background(), p.Endpoint, "audit", frozen)
				require.NoError(t, err)
				if scenario != "safe" {
					require.Equal(t, "failed", result.Status)
					require.Empty(t, result.Data)
					if scenario == "flagged" {
						require.Equal(t, "content_filtered", result.Error.Code)
					}
					return
				}
				require.Equal(t, "completed", result.Status)
				require.Len(t, result.Data, count)
				require.NotContains(t, result.Metadata.ProviderMetadata, "request_id")
				require.NotContains(t, result.Metadata.ProviderMetadata, "nsfw_content_detected")
			})
		}
	}
}

func TestStandardSafetyFlagAlsoBlocksDelivery(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/output-cardinality.json")
	require.NoError(t, err)
	var fixtures []sample
	require.NoError(t, json.Unmarshal(raw, &fixtures))
	checked := 0
	for _, f := range fixtures {
		var body map[string]any
		require.NoError(t, json.Unmarshal(f.Response, &body))
		if _, ok := body["has_nsfw_concepts"]; !ok {
			continue
		}
		var contract struct {
			Providers map[string]struct{ Upstream rv.ProviderSpec }
		}
		require.NoError(t, json.Unmarshal(f.Contract, &contract))
		p := contract.Providers["fal"].Upstream
		frozen, err := rv.FreezeProviderBinding(f.Contract, rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash})
		require.NoError(t, err)
		flags := make([]bool, len(body["images"].([]any)))
		flags[0] = true
		body["has_nsfw_concepts"] = flags
		data, _ := json.Marshal(body)
		client := fal.Client{BaseURL: "https://offline.invalid", HTTP: &http.Client{Transport: fixtureTransport{body: data}}}
		result, err := client.Poll(context.Background(), p.Endpoint, "audit", frozen)
		require.NoError(t, err)
		require.Equal(t, "failed", result.Status)
		require.Equal(t, "content_filtered", result.Error.Code)
		require.Empty(t, result.Data)
		checked++
	}
	require.Positive(t, checked)
}
