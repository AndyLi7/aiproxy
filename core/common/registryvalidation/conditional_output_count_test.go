//nolint:testpackage // White-box coverage needs the package-private resolveOutputCardinality fixture or implementation.
package registryvalidation

import (
	"encoding/json"
	"maps"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
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

			maps.Copy(changed, change)

			bytes := mustJSON(t, changed)
			_, err = resolveOutputCardinality(bytes, p.Input, body, 1024)
			require.Error(t, err)
		}

		props := checkedType[map[string]any](t, p.Input["properties"])

		name := "num_results"
		if declaration["rule"] == "image-series" {
			name = "series_amount"
		}

		field := checkedType[map[string]any](t, props[name])
		field["description"] = "Unreviewed source behavior"
		_, err = resolveOutputCardinality(p.Metering.Cardinality, p.Input, body, 1024)
		require.Error(t, err)
	}
}
