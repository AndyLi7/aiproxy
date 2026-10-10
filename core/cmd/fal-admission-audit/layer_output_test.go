package main

import (
	"context"
	"encoding/json"
	"fmt"
	rv "github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"github.com/stretchr/testify/require"
	"net/http"
	"os"
	"testing"
)

func TestLayerProjectionThroughFalParser(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/seedream-layer-output.json")
	require.NoError(t, err)
	var source map[string]any
	require.NoError(t, json.Unmarshal(raw, &source))
	binding := map[string]any{"provider": "fal", "id": "layer-test", "revision": "1", "contractHash": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	upstream := map[string]any{"id": binding["id"], "revision": binding["revision"], "contractHash": binding["contractHash"], "endpoint": "bytedance/seedream/v5/pro/layerize", "outputJsonSchema": source, "outputImages": map[string]any{"version": 3, "mode": "layers", "maximum": 17}}
	upstream["execution"] = map[string]any{"mode": "async", "statusEndpoint": "bytedance/seedream/v5/pro/layerize/requests/{request_id}/status", "resultEndpoint": "bytedance/seedream/v5/pro/layerize/requests/{request_id}", "supportsCancellation": false}
	contract := map[string]any{"provider_contract_version": 1, "selected_provider_binding": binding, "providers": map[string]any{"fal": map[string]any{"adapter": "fal-image", "upstream": upstream}}}
	for _, scenario := range []string{"one", "seventeen", "mismatch", "order", "overflow", "unbound", "drift"} {
		t.Run(scenario, func(t *testing.T) {
			n := 17
			if scenario == "one" {
				n = 1
			}
			if scenario == "overflow" {
				n = 18
			}
			images, layers := []any{}, []any{}
			for i := 0; i < n; i++ {
				image := map[string]any{"url": fmt.Sprintf("https://example.com/%d.png", i), "layer": map[string]any{"z_index": 999, "name": "forged"}}
				images = append(images, image)
				layers = append(layers, map[string]any{"image": image, "z_index": i, "name": nil, "description": "layer", "bounding_box": nil})
			}
			if scenario == "mismatch" {
				layers[1].(map[string]any)["image"] = map[string]any{"url": "https://example.com/wrong.png"}
			}
			if scenario == "order" {
				layers[1].(map[string]any)["z_index"] = 0
			}
			body, _ := json.Marshal(map[string]any{"images": images, "layers": layers})
			frozen, _ := json.Marshal(contract)
			if scenario == "drift" {
				upstream["outputImages"].(map[string]any)["maximum"] = 16
				frozen, _ = json.Marshal(contract)
				upstream["outputImages"].(map[string]any)["maximum"] = 17
			}
			if scenario == "one" || scenario == "seventeen" {
				_, e := rv.ExtractFrozenLayerMetadata(frozen, body)
				require.NoError(t, e)
			}
			client := fal.Client{BaseURL: "https://offline.invalid", HTTP: &http.Client{Transport: fixtureTransport{body: body}}}
			var contracts [][]byte
			if scenario != "unbound" {
				contracts = append(contracts, frozen)
			}
			result, err := client.Poll(context.Background(), "bytedance/seedream/v5/pro/layerize", "test", contracts...)
			require.NoError(t, err)
			if scenario == "mismatch" || scenario == "order" || scenario == "overflow" || scenario == "drift" {
				require.Equal(t, "failed", result.Status)
				require.Empty(t, result.Data)
				return
			}
			require.Equal(t, "completed", result.Status)
			require.Len(t, result.Data, n)
			if scenario == "unbound" {
				for _, image := range result.Data {
					require.Empty(t, image.Layer)
				}
				return
			}
			for i, image := range result.Data {
				var metadata map[string]any
				require.NoError(t, json.Unmarshal(image.Layer, &metadata))
				require.Equal(t, float64(i), metadata["z_index"])
				require.Nil(t, metadata["name"])
				require.NotContains(t, string(image.Layer), "https:")
				require.NotContains(t, string(image.Layer), "forged")
			}
			cloned := model.CloneImageOutputs(result.Data)
			cloned[0].Layer[0] = '!'
			require.NotEqual(t, cloned[0].Layer, result.Data[0].Layer)
			serialized, err := json.Marshal(result.Data)
			require.NoError(t, err)
			var restored []model.ImageOutput
			require.NoError(t, json.Unmarshal(serialized, &restored))
			require.Equal(t, result.Data, restored)
		})
	}
}
