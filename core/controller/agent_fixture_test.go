//nolint:testpackage // White-box helpers support the internal Agent contract fixtures.
package controller

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func agentFixtureJSON(t *testing.T, value any) []byte {
	t.Helper()

	raw, err := json.Marshal(value)
	require.NoError(t, err)

	return raw
}

func agentFixtureValue[T any](t *testing.T, value any) T {
	t.Helper()

	typed, ok := value.(T)
	require.True(t, ok, "unexpected fixture value type")

	return typed
}
