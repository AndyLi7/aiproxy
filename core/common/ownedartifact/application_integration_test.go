package ownedartifact

import (
	"bytes"
	"context"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

type appTransport func(*http.Request) (*http.Response, error)

func (f appTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Only the provider file download is stubbed. Upload/download cross the actual
// application handlers and local artifact service through loopback HTTP.
func TestNativeArtifactRealApplicationStorage(t *testing.T) {
	base := os.Getenv("NATIVE_ARTIFACT_TEST_URL")
	if base == "" {
		t.Skip("start disposable native-artifact-server.ts")
	}
	require.True(t, strings.HasPrefix(base, "http://127.0.0.1:"))
	t.Setenv("EXTERNAL_BALANCE_URL", base)
	t.Setenv("EXTERNAL_BALANCE_KEY", "local-native-test-only")
	for index, data := range [][]byte{[]byte("<svg><script>untrusted</script></svg>"), {80, 75, 3, 4, 0, 255}} {
		download := &http.Client{Transport: appTransport(func(r *http.Request) (*http.Response, error) {
			require.Equal(t, "https://provider.example/file", r.URL.String())
			return &http.Response{StatusCode: 200, ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data))}, nil
		})}
		receipt, err := store(context.Background(), "integration", index, "https://provider.example/file", base, "local-native-test-only", download, http.DefaultClient)
		require.NoError(t, err)
		require.True(t, ValidReceipt("integration", index, receipt))
		repeated, err := store(context.Background(), "integration", index, "https://provider.example/file", base, "local-native-test-only", download, http.DefaultClient)
		require.NoError(t, err)
		require.Equal(t, receipt, repeated)
		response, err := Download(context.Background(), receipt.Key)
		require.NoError(t, err)
		actual, err := io.ReadAll(response.Body)
		response.Body.Close()
		require.NoError(t, err)
		require.Equal(t, 200, response.StatusCode)
		require.Equal(t, data, actual)
		require.Equal(t, "application/octet-stream", response.Header.Get("Content-Type"))
		require.Contains(t, response.Header.Get("Content-Disposition"), "attachment")
		require.Contains(t, response.Header.Get("Content-Security-Policy"), "sandbox")
	}
}
