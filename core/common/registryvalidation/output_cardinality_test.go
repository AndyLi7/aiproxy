package registryvalidation

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestOutputCardinalityRejectsMalformedAndUnsafeBounds(t *testing.T) {
	schema := map[string]any{"properties": map[string]any{"num_layers": map[string]any{"type": "integer", "minimum": float64(1), "maximum": float64(10)}, "maps": map[string]any{"type": "array"}}}
	for _, tc := range []struct {
		raw  string
		body map[string]any
		want int
	}{
		{`{"version":1,"mode":"fixed","count":2}`, nil, 2},
		{`{"version":1,"mode":"parameter","parameter":"num_layers","maximum":10}`, map[string]any{"num_layers": float64(4)}, 4},
		{`{"version":1,"mode":"array-length","parameter":"maps","maximum":15}`, map[string]any{"maps": []any{"a", "b"}}, 2},
	} {
		n, err := resolveOutputCardinality(json.RawMessage(tc.raw), schema, tc.body, 1024)
		require.NoError(t, err)
		require.Equal(t, tc.want, n)
	}
	for _, raw := range []string{`null`, `{"version":1,"mode":"fixed","count":0}`, `{"version":1,"mode":"fixed","count":16}`, `{"version":1,"mode":"fixed","count":2,"extra":true}`, `{"version":1,"mode":"parameter","parameter":"num_layers","maximum":9}`, `{"version":1,"mode":"array-length","parameter":"num_layers","maximum":10}`} {
		_, err := resolveOutputCardinality(json.RawMessage(raw), schema, nil, 1024)
		require.Error(t, err)
	}
	for _, count := range []float64{0, 1.5, 11} {
		_, err := resolveOutputCardinality(json.RawMessage(`{"version":1,"mode":"parameter","parameter":"num_layers","maximum":10}`), schema, map[string]any{"num_layers": count}, 1024)
		require.Error(t, err)
	}
}
