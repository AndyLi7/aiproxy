package middleware

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

// The shared fixture (core/model/testdata/public-api-id-v1.json, a
// byte-identical copy of the application's tests/fixtures/public-api-id-v1.json)
// states the exact error bodies the application documents. The distributor
// answers them byte-for-byte (as JSON) for a key that may call every config.
func TestPublicAPIIDFixtureErrorBodies(t *testing.T) {
	raw, err := os.ReadFile("../model/testdata/public-api-id-v1.json")
	require.NoError(t, err)

	type example struct {
		Requested  string          `json:"requested"`
		GatewayEnv string          `json:"gateway_env"`
		Status     int             `json:"status"`
		Body       json.RawMessage `json:"body"`
	}

	original := config.DisableImageGroupIDs
	t.Cleanup(func() { config.DisableImageGroupIDs = original })

	var fixture struct {
		Configs      []model.ModelConfig `json:"configs"`
		NativeErrors []example           `json:"native_errors"`
		ImageErrors  []example           `json:"image_errors"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.NotEmpty(t, fixture.NativeErrors)
	require.NotEmpty(t, fixture.ImageErrors)

	entitled := make([]string, 0, len(fixture.Configs))
	for _, entry := range fixture.Configs {
		entitled = append(entitled, entry.Model)
	}

	for _, tc := range fixture.NativeErrors {
		body, err := json.Marshal(map[string]any{"model": tc.Requested, "input": map[string]any{}})
		require.NoError(t, err)

		got := distributeAs(t, mode.NativeTasks, "/v1/model-tasks", fixture.Configs, entitled, false, string(body))
		require.False(t, got.reached, tc.Requested)
		require.Equal(t, tc.Status, got.status, "%s: %s", tc.Requested, got.body)
		require.JSONEq(t, string(tc.Body), got.body, tc.Requested)
	}

	for _, tc := range fixture.ImageErrors {
		switch tc.GatewayEnv {
		case "":
			config.DisableImageGroupIDs = false
		case "DISABLE_IMAGE_GROUP_IDS=true":
			config.DisableImageGroupIDs = true
		default:
			t.Fatalf("%s: unknown gateway_env %q", tc.Requested, tc.GatewayEnv)
		}

		body, err := json.Marshal(map[string]any{"model": tc.Requested, "prompt": "a cat"})
		require.NoError(t, err)

		for _, path := range []string{"/v1/images/generations", "/v1/images/tasks"} {
			got := distributeAs(t, mode.ImagesGenerations, path, fixture.Configs, entitled, false, string(body))
			require.False(t, got.reached, tc.Requested)
			require.Equal(t, tc.Status, got.status, "%s %s: %s", path, tc.Requested, got.body)
			require.JSONEq(t, string(tc.Body), got.body, "%s %s", path, tc.Requested)
		}
	}
}
