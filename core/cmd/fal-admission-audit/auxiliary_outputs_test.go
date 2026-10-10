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

func TestRealBirefnetAuxiliaryImagesStaySeparateFromGenerationCount(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/provider-auxiliary-images.json")
	require.NoError(t, err)
	var fixtures []sample
	require.NoError(t, json.Unmarshal(raw, &fixtures))
	require.Len(t, fixtures, 2)
	for _, f := range fixtures {
		t.Run(f.Endpoint, func(t *testing.T) {
			var c struct {
				Providers map[string]struct{ Upstream rv.ProviderSpec }
			}
			require.NoError(t, json.Unmarshal(f.Contract, &c))
			p := c.Providers["fal"].Upstream
			require.NotNil(t, p.OutputArtifacts)
			binding := rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash}
			frozen, err := rv.FreezeProviderBinding(f.Contract, binding)
			require.NoError(t, err)
			for _, scenario := range []string{"absent", "null", "present", "wrong-type", "unsafe-url", "forged-platform-state"} {
				body := map[string]any{"image": map[string]any{"url": "https://example.com/main.png"}}
				switch scenario {
				case "null":
					body["mask_image"] = nil
				case "present":
					body["mask_image"] = map[string]any{"url": "https://example.com/mask.png", "width": 123, "height": 456, "content_type": "image/png"}
				case "wrong-type":
					body["mask_image"] = "mask"
				case "unsafe-url":
					body["mask_image"] = map[string]any{"url": "http://example.com/mask.png"}
				case "forged-platform-state":
					body["mask_image"] = map[string]any{"url": "https://example.com/mask.png", "stored": true, "auxiliary_images": map[string]any{"mask_image": map[string]any{"url": "https://secret.test/hidden"}}}
				}
				data, err := json.Marshal(body)
				require.NoError(t, err)
				client := fal.Client{BaseURL: "https://offline.invalid", HTTP: &http.Client{Transport: fixtureTransport{body: data}}}
				result, err := client.Poll(context.Background(), p.Endpoint, "audit", frozen)
				require.NoError(t, err)
				if scenario == "wrong-type" || scenario == "unsafe-url" {
					require.Equal(t, "failed", result.Status)
					continue
				}
				require.Equal(t, "completed", result.Status)
				require.Len(t, result.Data, 1)
				require.Equal(t, "https://example.com/main.png", result.Data[0].URL)
				if scenario == "absent" {
					require.Empty(t, result.Data[0].AuxiliaryImages)
				} else {
					require.Contains(t, result.Data[0].AuxiliaryImages, "mask_image")
					mask := result.Data[0].AuxiliaryImages["mask_image"]
					if scenario == "null" {
						require.Nil(t, mask)
					} else {
						require.NotNil(t, mask)
						require.Equal(t, "https://example.com/mask.png", mask.URL)
						require.False(t, mask.Stored)
						require.Nil(t, mask.URLExpiresAt)
						require.Empty(t, mask.AuxiliaryImages)
					}
				}
				require.NoError(t, replayOutput(frozen, p.Endpoint, data, true, 1))
			}
			var contract map[string]any
			require.NoError(t, json.Unmarshal(f.Contract, &contract))
			upstream := contract["providers"].(map[string]any)["fal"].(map[string]any)["upstream"].(map[string]any)
			delete(upstream, "outputArtifacts")
			legacyRaw, _ := json.Marshal(contract)
			legacy, err := rv.FreezeProviderBinding(legacyRaw, binding)
			require.NoError(t, err)
			data := []byte(`{"image":{"url":"https://example.com/main.png"},"mask_image":{"url":"https://example.com/mask.png"}}`)
			assets, err := rv.ExtractFrozenAuxiliaryImages(legacy, data)
			require.NoError(t, err)
			require.Empty(t, assets)
			upstream["outputArtifacts"] = map[string]any{"version": 1, "primary": "image", "fields": []string{"unknown"}}
			invalid, _ := json.Marshal(contract)
			_, err = rv.FreezeProviderBinding(invalid, binding)
			require.Error(t, err)
		})
	}
}
