package trace

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTrustedKeysDecodeConfiguredKeyAndEmptyDefault(t *testing.T) {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	raw, err := json.Marshal(map[string]string{"app-1": base64.StdEncoding.EncodeToString(key)})
	require.NoError(t, err)
	keys, err := ParseTrustedKeys(string(raw))
	require.NoError(t, err)
	require.Equal(t, key, keys["app-1"])
	keys, err = ParseTrustedKeys("")
	require.NoError(t, err)
	require.Empty(t, keys)
}
