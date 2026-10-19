//nolint:testpackage // White-box coverage needs the package-private patterned fixture or implementation.
package ownedartifact

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This explicit opt-in fixture uses real TLS provider HTTP, application HTTP and
// signed S3 HTTP. The test transport alone maps provider.example to loopback.
func TestStreamingArchiveRehearsal(t *testing.T) {
	app, root := os.Getenv("NATIVE_ARTIFACT_TEST_URL"), os.Getenv("STREAMING_REHEARSAL_DIR")
	if app == "" || root == "" {
		t.Skip("requires isolated streaming rehearsal fixtures")
	}

	require.True(t, strings.HasPrefix(app, "http://127.0.0.1:"))

	sizeMiB, err := strconv.Atoi(os.Getenv("STREAMING_REHEARSAL_MIB"))
	require.NoError(t, err)
	require.True(t, sizeMiB == 32 || sizeMiB == 200)
	size := int64(sizeMiB) << 20
	head := []byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00")
	file, err := os.CreateTemp(t.TempDir(), "source-")
	require.NoError(t, err)

	defer file.Close()

	_, err = io.Copy(file, &patterned{head: head, size: size})
	require.NoError(t, err)

	sum, chunks := patternedDigests(head, size)

	provider := httptest.NewTLSServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f, err := os.Open(file.Name())
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			defer f.Close()

			http.ServeContent(w, r, "source.mp4", time.Time{}, f)
		}),
	)
	defer provider.Close()

	target, err := url.Parse(provider.URL)
	require.NoError(t, err)

	transportBase := provider.Client().Transport
	download := &http.Client{Transport: transport(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme = target.Scheme
		clone.URL.Host = target.Host
		clone.Host = target.Host

		return transportBase.RoundTrip(clone)
	})}

	runtime.GC()

	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	finishMemory := rehearsalHeapPeak()
	t.Cleanup(func() { finishMemory() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	receipt, err := store(
		ctx,
		"req",
		0,
		"https://provider.example/source.mp4",
		app,
		"local-native-test-only",
		download,
		uploadClient(),
	)
	require.NoError(t, err)
	require.Equal(t, size, receipt.Size)
	require.Equal(t, sum, receipt.SHA256)
	require.Equal(t, chunks, receipt.Chunks)
	require.Equal(t, "video/mp4", receipt.Type)
	raw, err := json.Marshal(receipt)
	require.NoError(t, err)
	// #nosec G703 -- The opt-in local integration harness owns this fixture or rendezvous path; no HTTP input supplies it.
	require.NoError(t, os.WriteFile(filepath.Join(root, "receipt.json"), raw, 0o600))

	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)

	var usage syscall.Rusage
	require.NoError(t, syscall.Getrusage(syscall.RUSAGE_SELF, &usage))
	t.Logf(
		"ARCHIVE sizeMiB=%d baselineHeap=%d heap=%d peakHeap=%d sys=%d maxRSS=%d sha256=%s",
		sizeMiB,
		baseline.HeapAlloc,
		memory.HeapAlloc,
		finishMemory(),
		memory.Sys,
		usage.Maxrss,
		sum,
	)
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

	var (
		once sync.Once
		peak uint64
	)

	return func() uint64 { once.Do(func() { close(stop); peak = <-result }); return peak }
}
