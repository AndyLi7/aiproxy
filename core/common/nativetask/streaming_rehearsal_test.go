package nativetask

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestStreamingDownloadRehearsal(t *testing.T) {
	app, root := os.Getenv("NATIVE_ARTIFACT_TEST_URL"), os.Getenv("STREAMING_REHEARSAL_DIR")
	if app == "" || root == "" {
		t.Skip("requires isolated streaming rehearsal fixtures")
	}
	require.True(t, strings.HasPrefix(app, "http://127.0.0.1:"))
	// Archive/download use the actual Nitro HTTP listener; the browser relay
	// fixture is a separate loopback service, never an upload proxy.
	relay := os.Getenv("NATIVE_ARTIFACT_RELAY_URL")
	if relay == "" {
		relay = app
	}
	require.True(t, strings.HasPrefix(relay, "http://127.0.0.1:"))
	t.Setenv("EXTERNAL_BALANCE_URL", app)
	t.Setenv("EXTERNAL_BALANCE_KEY", "local-native-test-only")
	raw, err := os.ReadFile(filepath.Join(root, "receipt.json"))
	require.NoError(t, err)
	var receipt ownedartifact.Receipt
	require.NoError(t, json.Unmarshal(raw, &receipt))
	e, plan, _, provider := setup(t)
	var contract nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(plan.Contract, &contract))
	contract.Artifacts = []nativeresult.ArtifactBinding{{Path: []string{"file", "url"}}}
	plan.Contract, _ = json.Marshal(contract)
	plan.DeliveryBase = "https://gateway.example"
	_, err = e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.NoError(t, model.SaveNativeTaskResult(e.DB, "req", "g", 1, []byte(`{"file":{"url":"https://provider.example/source.mp4","file_name":"provider.mp4"}}`)))
	done, err := e.Deliver(context.Background(), "req", "g", 1, func(context.Context, string, int, string) (ownedartifact.Receipt, error) { return receipt, nil })
	require.NoError(t, err)
	require.Equal(t, "completed", done.Status)
	require.Equal(t, 1, provider.calls)
	h := &HTTP{Engine: e, Identity: func(*http.Request) (string, int, error) { return "g", 1, nil }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.GetArtifact(w, r, "req", "0") }))
	defer server.Close()
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	finishMemory := rehearsalHeapPeak()
	t.Cleanup(func() { finishMemory() })
	// Read both the public gateway and the application's browser relay in full.
	for _, endpoint := range []string{server.URL, relay + "/relay?target=" + url.QueryEscape(server.URL)} {
		response, err := http.Get(endpoint)
		require.NoError(t, err)
		require.Equal(t, 200, response.StatusCode)
		require.Equal(t, receipt.Size, response.ContentLength)
		hash := sha256.New()
		n, err := io.Copy(hash, response.Body)
		response.Body.Close()
		require.NoError(t, err)
		require.Equal(t, receipt.Size, n)
		require.Equal(t, receipt.SHA256, hex.EncodeToString(hash.Sum(nil)))
		require.Equal(t, "video/mp4", response.Header.Get("Content-Type"))
		require.Contains(t, response.Header.Get("Content-Disposition"), "req.mp4")
		for _, spec := range []struct {
			header     string
			start, end int64
		}{{"bytes=0-15", 0, 15}, {"bytes=2097149-2097160", 2097149, 2097160}, {fmt.Sprintf("bytes=%d-", receipt.Size-100), receipt.Size - 100, receipt.Size - 1}, {"bytes=-67", receipt.Size - 67, receipt.Size - 1}} {
			req, _ := http.NewRequest("GET", endpoint, nil)
			req.Header.Set("Range", spec.header)
			part, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			require.Equal(t, 206, part.StatusCode)
			require.Equal(t, fmt.Sprintf("bytes %d-%d/%d", spec.start, spec.end, receipt.Size), part.Header.Get("Content-Range"))
			require.Equal(t, spec.end-spec.start+1, part.ContentLength)
			got, err := io.ReadAll(part.Body)
			part.Body.Close()
			require.NoError(t, err)
			require.Len(t, got, int(spec.end-spec.start+1))
			head := []byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00")
			for i, b := range got {
				offset := spec.start + int64(i)
				want := byte(offset * 31)
				if offset < int64(len(head)) {
					want = head[offset]
				}
				require.Equal(t, want, b)
			}
		}
		req, _ := http.NewRequest("GET", endpoint, nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", receipt.Size))
		bad, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, 416, bad.StatusCode)
		require.Equal(t, fmt.Sprintf("bytes */%d", receipt.Size), bad.Header.Get("Content-Range"))
		bad.Body.Close()
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	var usage syscall.Rusage
	require.NoError(t, syscall.Getrusage(syscall.RUSAGE_SELF, &usage))
	t.Logf("DOWNLOAD size=%d baselineHeap=%d heap=%d peakHeap=%d sys=%d maxRSS=%d", receipt.Size, baseline.HeapAlloc, memory.HeapAlloc, finishMemory(), memory.Sys, usage.Maxrss)
}

// Sample the live Go heap every 10 ms, independently of file size. RSS below
// uses the kernel process high-water mark (bytes on this Darwin rehearsal).
func rehearsalHeapPeak() func() uint64 {
	stop := make(chan struct{})
	result := make(chan uint64, 1)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		var peak uint64
		sample := func() {
			var memory runtime.MemStats
			runtime.ReadMemStats(&memory)
			if memory.HeapAlloc > peak {
				peak = memory.HeapAlloc
			}
		}
		sample()
		for {
			select {
			case <-ticker.C:
				sample()
			case <-stop:
				sample()
				result <- peak
				return
			}
		}
	}()
	var once sync.Once
	var peak uint64
	return func() uint64 { once.Do(func() { close(stop); peak = <-result }); return peak }
}
