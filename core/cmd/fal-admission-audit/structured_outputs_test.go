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

func TestRealStructuredOutputDocumentsAndCombinedImages(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/provider-structured-outputs.json")
	require.NoError(t, err)
	var fixtures []sample
	require.NoError(t, json.Unmarshal(raw, &fixtures))
	require.Len(t, fixtures, 13)
	for _, f := range fixtures {
		t.Run(f.Endpoint, func(t *testing.T) {
			var c struct {
				Providers map[string]struct{ Upstream rv.ProviderSpec }
			}
			require.NoError(t, json.Unmarshal(f.Contract, &c))
			p := c.Providers["fal"].Upstream
			binding := rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash}
			frozen, err := rv.FreezeProviderBinding(f.Contract, binding)
			require.NoError(t, err)
			var body map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(f.Response, &body))
			require.NotNil(t, body["image"])
			document := json.RawMessage(`{"objects":[{"id":18446744073709551615,"weight":0.1234567890123456789,"label":"generated scene","mask":null}],"nested":{"enabled":true}}`)
			for _, field := range p.OutputMetadata.Fields {
				body[field] = document
			}
			for _, scenario := range []string{"omitted", "empty", "duplicate", "distinct", "conflicting"} {
				delete(body, "images")
				switch scenario {
				case "empty":
					body["images"] = json.RawMessage(`[]`)
				case "duplicate":
					body["images"] = append(append(json.RawMessage(`[`), body["image"]...), ']')
				case "distinct":
					body["images"] = json.RawMessage(`[{"url":"https://example.com/second.png"}]`)
				case "conflicting":
					var image map[string]any
					require.NoError(t, json.Unmarshal(body["image"], &image))
					image["width"] = 456789
					array, _ := json.Marshal([]any{image})
					body["images"] = array
				}
				data, _ := json.Marshal(body)
				client := fal.Client{BaseURL: "https://offline.invalid", HTTP: &http.Client{Transport: fixtureTransport{body: data}}}
				result, err := client.Poll(context.Background(), p.Endpoint, "audit", frozen)
				require.NoError(t, err)
				if scenario == "conflicting" {
					require.Equal(t, "failed", result.Status)
					continue
				}
				require.Equal(t, "completed", result.Status)
				count := 1
				if scenario == "distinct" {
					count = 2
				}
				require.Len(t, result.Data, count)
				for _, field := range p.OutputMetadata.Fields {
					require.Equal(t, document, result.Metadata.ProviderMetadata[field])
				}
				if scenario == "distinct" {
					require.Error(t, replayOutput(frozen, p.Endpoint, data, true, 1))
				}
			}
			delete(body, "image")
			data, _ := json.Marshal(body)
			require.Error(t, rv.ValidateFrozenProviderOutput(frozen, data))
			// A legacy contract must not silently opt in to combined image handling.
			var contract map[string]any
			require.NoError(t, json.Unmarshal(f.Contract, &contract))
			delete(contract["providers"].(map[string]any)["fal"].(map[string]any)["upstream"].(map[string]any), "outputImages")
			legacyRaw, _ := json.Marshal(contract)
			legacy, err := rv.FreezeProviderBinding(legacyRaw, binding)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(f.Response, &body))
			body["images"] = json.RawMessage(`[]`)
			for _, field := range p.OutputMetadata.Fields {
				body[field] = document
			}
			data, _ = json.Marshal(body)
			require.NoError(t, replayOutput(legacy, p.Endpoint, data, false))
		})
	}
}
