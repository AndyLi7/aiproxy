package nativetask

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// A client that stops reading must not hold a download slot: the transfer
// ends at its deadline and the slot is free again.
func TestStalledArtifactReaderIsCutOffAndFreesItsSlot(t *testing.T) {
	previous := artifactTransferTime
	artifactTransferTime = func(int64) time.Duration { return 300 * time.Millisecond }
	t.Cleanup(func() { artifactTransferTime = previous })

	e, plan, _, _ := setup(t)
	ctx := context.Background()
	var contract nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(plan.Contract, &contract))
	contract.Artifacts = []nativeresult.ArtifactBinding{{Path: []string{"file", "url"}}}
	plan.Contract, _ = json.Marshal(contract)
	plan.DeliveryBase = "https://gateway.example"
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.NoError(t, model.SaveNativeTaskResult(e.DB, "req", "g", 1, []byte(`{"file":{"url":"https://provider.example/file.bin"}}`)))
	payload := bytes.Repeat([]byte("a"), 12<<20)
	digest := sha256.Sum256(payload)
	key := "native-results/req/0/" + strings.Repeat("a", 64) + ".bin"
	_, err = e.Deliver(ctx, "req", "g", 1, func(context.Context, string, int, string) (ownedartifact.Receipt, error) {
		return ownedartifact.Receipt{Key: key, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(payload))}, nil
	})
	require.NoError(t, err)

	limiter := newReadLimiter(time.Now)
	h := &HTTP{Engine: e, Identity: func(*http.Request) (string, int, error) { return "g", 1, nil }, limiter: limiter,
		Download: func(*http.Request, string) (*http.Response, error) {
			return &http.Response{StatusCode: 200, ContentLength: int64(len(payload)), Body: io.NopCloser(bytes.NewReader(payload))}, nil
		}}
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.GetArtifact(w, r, "req", "0")
		close(done)
	}))
	defer server.Close()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("GET /v1/model-tasks/req/artifacts/0 HTTP/1.1\r\nHost: gateway.test\r\n\r\n"))
	require.NoError(t, err)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled reader kept its download slot")
	}
	for range artifactDownloadsPerKey {
		_, ok := limiter.acquireDownload("g", 1)
		require.True(t, ok, "every slot of the key is free again")
	}
}
