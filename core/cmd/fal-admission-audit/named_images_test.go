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

func TestActualMidasNamedImages(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/midas-named-images.json")
	require.NoError(t, err)
	var fixtures []sample
	require.NoError(t, json.Unmarshal(raw, &fixtures))
	require.Len(t, fixtures, 2)
	for _, f := range fixtures {
		r := run(f)
		require.True(t, r.Passed, r.Error)
		require.True(t, r.OutputPassed, r.OutputError)
		var c struct {
			Providers map[string]struct{ Upstream rv.ProviderSpec }
		}
		require.NoError(t, json.Unmarshal(f.Contract, &c))
		p := c.Providers["fal"].Upstream
		frozen, err := rv.FreezeProviderBinding(f.Contract, rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash})
		require.NoError(t, err)
		for _, scenario := range []string{"same-url", "missing", "null", "bad-url", "forged-name"} {
			t.Run(f.Scenario+"/"+scenario, func(t *testing.T) {
				image := map[string]any{"url": "https://example.com/shared.png"}
				response := map[string]any{"depth_map": image, "normal_map": image}
				switch scenario {
				case "missing":
					delete(response, "normal_map")
				case "null":
					response["normal_map"] = nil
				case "bad-url":
					response["normal_map"] = map[string]any{"url": "file:///tmp/private"}
				case "forged-name":
					image["source_field"] = "forged"
				}
				body, _ := json.Marshal(response)
				client := fal.Client{BaseURL: "https://offline.invalid", HTTP: &http.Client{Transport: fixtureTransport{body: body}}}
				result, err := client.Poll(context.Background(), p.Endpoint, "audit", frozen)
				require.NoError(t, err)
				if scenario == "missing" || scenario == "null" || scenario == "bad-url" {
					require.Equal(t, "failed", result.Status)
					return
				}
				require.Equal(t, "completed", result.Status)
				require.Len(t, result.Data, 2)
				require.Equal(t, "depth_map", result.Data[0].SourceField)
				require.Equal(t, "normal_map", result.Data[1].SourceField)
			})
		}
	}
}
