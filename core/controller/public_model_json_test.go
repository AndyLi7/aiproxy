package controller

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPublicModelJSONOmitsLegacyPermissions(t *testing.T) {
	encoded, err := json.Marshal(OpenAIModels{ID: "vendor/image", Object: "model", OwnedBy: "vendor", Created: 1, Root: "private-root", Permission: permission})
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(encoded, &body))
	require.Len(t, body, 4)
	require.Equal(t, "vendor/image", body["id"])
	require.NotContains(t, string(encoded), "modelperm")
}
