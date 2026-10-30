//nolint:testpackage // White-box coverage needs the package-private newReadLimiter fixture or implementation.
package nativetask

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/stretchr/testify/require"
)

func TestStatusReadsAreRateLimitedPerKey(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)

	l := newReadLimiter(func() time.Time { return now })
	for i := range statusReadBurst {
		require.True(t, l.allowStatus("g", 1), i)
	}

	require.False(t, l.allowStatus("g", 1))
	// Another key is unaffected.
	require.True(t, l.allowStatus("g", 2))

	now = now.Add(time.Second)

	for i := range statusReadsPerSecond {
		require.True(t, l.allowStatus("g", 1), i)
	}

	require.False(t, l.allowStatus("g", 1))
}

func TestArtifactDownloadsAreBoundedPerKeyAndGlobally(t *testing.T) {
	l := newReadLimiter(time.Now)

	releases := make([]func(), 0, artifactDownloadsPerKey)
	for range artifactDownloadsPerKey {
		release, ok := l.acquireDownload("g", 1)
		require.True(t, ok)

		releases = append(releases, release)
	}

	_, ok := l.acquireDownload("g", 1)
	require.False(t, ok)
	releases[0]()
	releases[0]() // releasing twice frees one slot only

	release, ok := l.acquireDownload("g", 1)
	require.True(t, ok)
	_, ok = l.acquireDownload("g", 1)
	require.False(t, ok)
	release()

	for _, r := range releases[1:] {
		r()
	}
	// One account's keys share a group cap below the global one.
	groupReleases := make([]func(), 0, artifactDownloadsPerGroup)
	for i := range artifactDownloadsPerGroup {
		release, ok := l.acquireDownload("g", 100+i)
		require.True(t, ok)

		groupReleases = append(groupReleases, release)
	}

	_, ok = l.acquireDownload("g", 999)
	require.False(t, ok)

	for _, r := range groupReleases {
		r()
	}

	for i := range artifactDownloadsGlobal {
		_, ok := l.acquireDownload("group-"+strconv.Itoa(i), 1)
		require.True(t, ok)
	}

	_, ok = l.acquireDownload("another", 1)
	require.False(t, ok)
}

func TestStatusReadOverLimitReturns429WithoutReadingTheTask(t *testing.T) {
	e, _, _, _ := setup(t)
	now := time.Unix(1_800_000_000, 0)

	h := &HTTP{
		Engine:   e,
		Identity: func(*http.Request) (string, int, error) { return "g", 1, nil },
		limiter:  newReadLimiter(func() time.Time { return now }),
	}
	for range statusReadBurst {
		w := httptest.NewRecorder()
		h.Get(
			w,
			httptest.NewRequestWithContext(
				t.Context(),
				http.MethodGet,
				"/v1/model-tasks/missing",
				nil,
			),
			"missing",
		)
		require.Equal(t, 404, w.Code)
	}

	w := httptest.NewRecorder()
	h.Get(
		w,
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/model-tasks/missing", nil),
		"missing",
	)
	require.Equal(t, 429, w.Code)
	require.Contains(t, w.Body.String(), "rate_limit_exceeded")
}

func TestLargeArtifactsStreamAndNeverDeliverACorruptedTail(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), (bufferedArtifactLimit/16)+4096)
	sum := sha256.Sum256(data)
	receipt := ownedartifact.Receipt{
		Key:    "k",
		Size:   int64(len(data)),
		SHA256: hex.EncodeToString(sum[:]),
	}
	file := artifactFile{name: "task.bin", receipt: receipt}

	ok := httptest.NewRecorder()
	streamArtifact(ok, bytes.NewReader(data), file)
	require.Equal(t, 200, ok.Code)
	require.Equal(t, data, ok.Body.Bytes())

	corrupted := append([]byte(nil), data...)
	corrupted[len(corrupted)/2] ^= 0xff
	bad := httptest.NewRecorder()
	streamArtifact(bad, bytes.NewReader(corrupted), file)
	// The declared length is never reached, so the client sees a failed transfer.
	require.Equal(t, receipt.Size, mustParseLength(t, bad.Header().Get("Content-Length")))
	require.Less(t, int64(bad.Body.Len()), receipt.Size)
	require.Equal(t, int64(len(data)-artifactTailHoldback), int64(bad.Body.Len()))

	short := httptest.NewRecorder()
	streamArtifact(short, bytes.NewReader(data[:len(data)-1]), file)
	require.Less(t, int64(short.Body.Len()), receipt.Size-1)
}

func mustParseLength(t *testing.T, value string) int64 {
	t.Helper()

	var n int64
	for _, c := range value {
		require.True(t, c >= '0' && c <= '9')
		n = n*10 + int64(c-'0')
	}

	return n
}

func TestImageWaitConcurrencyIsPerOwnerKey(t *testing.T) {
	l := newReadLimiter(time.Now)

	releases := make([]func(), 0, imageWaitsPerKey)
	for range imageWaitsPerKey {
		release, ok := l.acquireImageWait("g", 1)
		require.True(t, ok)

		releases = append(releases, release)
	}

	_, ok := l.acquireImageWait("g", 1)
	require.False(t, ok)
	otherKey, ok := l.acquireImageWait("g", 2)
	require.True(t, ok)
	otherOwner, ok := l.acquireImageWait("other", 1)
	require.True(t, ok)
	otherKey()
	otherOwner()

	for _, release := range releases {
		release()
		release()
	}

	require.Empty(t, l.waits)
}
