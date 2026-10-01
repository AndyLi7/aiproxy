package registryvalidation

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestConditionalCountRequiresExactSourceEvidence(t *testing.T) {
	raw, err := os.ReadFile("testdata/conditional-count.json")
	require.NoError(t, err)
	var rows []struct {
		Contract struct {
			Providers map[string]struct {
				Upstream struct {
					Input    map[string]any `json:"inputJsonSchema"`
					Metering struct {
						Cardinality json.RawMessage `json:"outputCardinality"`
					} `json:"metering"`
				} `json:"upstream"`
			} `json:"providers"`
		} `json:"contract"`
		Request map[string]any `json:"request"`
	}
	require.NoError(t, json.Unmarshal(raw, &rows))
	for _, row := range rows {
		p := row.Contract.Providers["fal"].Upstream
		body := row.Request
		if n, ok := body["n"]; ok {
			body["num_images"] = n
		}
		_, err := resolveOutputCardinality(p.Metering.Cardinality, p.Input, body, 1024)
		require.NoError(t, err)
		var declaration map[string]any
		require.NoError(t, json.Unmarshal(p.Metering.Cardinality, &declaration))
		for _, change := range []map[string]any{{"maximum": 100}, {"rule": "unknown"}, {"version": 4}, {"extra": true}} {
			var changed map[string]any
			require.NoError(t, json.Unmarshal(p.Metering.Cardinality, &changed))
			for k, v := range change {
				changed[k] = v
			}
			bytes, _ := json.Marshal(changed)
			_, err = resolveOutputCardinality(bytes, p.Input, body, 1024)
			require.Error(t, err)
		}
		props := p.Input["properties"].(map[string]any)
		name := "num_results"
		if declaration["rule"] == "image-series" {
			name = "series_amount"
		}
		field := props[name].(map[string]any)
		field["description"] = "Unreviewed source behavior"
		_, err = resolveOutputCardinality(p.Metering.Cardinality, p.Input, body, 1024)
		require.Error(t, err)
	}
}
