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

func TestBriaMaskAliasDeliveredWithoutExtraImageCount(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/bria-mask-alias.json")
	require.NoError(t, err)
	var fixtures []sample
	require.NoError(t, json.Unmarshal(raw, &fixtures))
	require.Len(t, fixtures, 6)
	for _, f := range fixtures {
		checked := run(f)
		require.True(t, checked.Passed, checked.Error)
		require.True(t, checked.OutputPassed, checked.OutputError)
		var c struct {
			Providers map[string]struct{ Upstream rv.ProviderSpec }
		}
		require.NoError(t, json.Unmarshal(f.Contract, &c))
		p := c.Providers["fal"].Upstream
		frozen, err := rv.FreezeProviderBinding(f.Contract, rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash})
		require.NoError(t, err)
		for _, scenario := range []string{"present", "absent", "null", "unsafe", "forged"} {
			body := map[string]any{"image": map[string]any{"url": "https://example.com/main.png"}, "mask": map[string]any{"url": "https://example.com/mask.png"}}
			switch scenario {
			case "absent":
				delete(body, "mask")
			case "null":
				body["mask"] = nil
			case "unsafe":
				body["mask"] = map[string]any{"url": "http://example.com/mask.png"}
			case "forged":
				body["mask"] = map[string]any{"url": "https://example.com/mask.png", "stored": true, "source_field": "forged", "map_type": "normal", "seed": 4, "revised_prompt": "private"}
				body["mask_image"] = map[string]any{"url": "https://example.com/forged.png"}
			}
			data, _ := json.Marshal(body)
			client := fal.Client{BaseURL: "https://offline.invalid", HTTP: &http.Client{Transport: fixtureTransport{body: data}}}
			result, err := client.Poll(context.Background(), p.Endpoint, "audit", frozen)
			require.NoError(t, err)
			if scenario == "absent" || scenario == "null" || scenario == "unsafe" {
				require.Equal(t, "failed", result.Status)
				require.Empty(t, result.Data)
				continue
			}
			require.Equal(t, "completed", result.Status)
			require.Len(t, result.Data, 1)
			require.Len(t, result.Data[0].AuxiliaryImages, 1)
			mask := result.Data[0].AuxiliaryImages["mask_image"]
			require.NotNil(t, mask)
			require.Equal(t, "https://example.com/mask.png", mask.URL)
			require.False(t, mask.Stored)
			require.Empty(t, mask.SourceField)
			require.Empty(t, mask.MapType)
			require.Nil(t, mask.Seed)
			require.Empty(t, mask.RevisedPrompt)
			require.NoError(t, replayOutput(frozen, p.Endpoint, data, true, 1))
		}
	}
}
