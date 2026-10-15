package ownedartifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// patterned yields size bytes that start with head, without holding them.
type patterned struct {
	head []byte
	size int64
	read int64
}

func (p *patterned) Read(b []byte) (int, error) {
	if p.read >= p.size {
		return 0, io.EOF
	}
	n := int64(len(b))
	if remaining := p.size - p.read; n > remaining {
		n = remaining
	}
	for i := int64(0); i < n; i++ {
		offset := p.read + i
		if offset < int64(len(p.head)) {
			b[i] = p.head[offset]
		} else {
			b[i] = byte(offset * 31)
		}
	}
	p.read += n
	return int(n), nil
}

func patternedDigests(head []byte, size int64) (string, string) {
	whole, chunk := sha256.New(), sha256.New()
	chunks := ""
	source := &patterned{head: head, size: size}
	buffer := make([]byte, ChunkSize)
	for {
		n, err := io.ReadFull(source, buffer)
		if n > 0 {
			whole.Write(buffer[:n])
			chunk.Reset()
			chunk.Write(buffer[:n])
			chunks += hex.EncodeToString(chunk.Sum(nil))
		}
		if err != nil {
			break
		}
	}
	return hex.EncodeToString(whole.Sum(nil)), chunks
}

// application stands in for the storage endpoint: it hashes what it receives
// and answers like the real handler.
func application(t *testing.T, seen *http.Header) *http.Client {
	return &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		*seen = r.Header.Clone()
		digest := sha256.New()
		n, err := io.Copy(digest, r.Body)
		if err != nil {
			return nil, err
		}
		sum := hex.EncodeToString(digest.Sum(nil))
		raw, _ := json.Marshal(map[string]any{"code": 0, "data": Receipt{Key: "native-results/task/2/" + sum + ".bin", SHA256: sum, Size: n}})
		return response(200, string(raw)), nil
	})}
}

func source(head []byte, size, declared int64) *http.Client {
	return &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: declared, Body: io.NopCloser(&patterned{head: head, size: size}), Header: make(http.Header)}, nil
	})}
}

func TestArchiveStreamsAndRecordsChunksAndType(t *testing.T) {
	head := []byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00")
	size := int64(3*ChunkSize + 777)
	sum, chunks := patternedDigests(head, size)
	for _, declared := range []int64{size} {
		var seen http.Header
		receipt, err := store(context.Background(), "task", 2, "https://provider.example/out.mp4", "http://app", "key", source(head, size, declared), application(t, &seen))
		require.NoError(t, err, declared)
		require.Equal(t, sum, receipt.SHA256)
		require.Equal(t, size, receipt.Size)
		require.Equal(t, "video/mp4", receipt.Type)
		require.Equal(t, chunks, receipt.Chunks)
		require.Len(t, receipt.Chunks, 4*64)
		require.True(t, ValidReceipt("task", 2, receipt))
		require.Equal(t, "video/mp4", seen.Get("X-Native-Artifact-Type"))
		require.Equal(t, "application/octet-stream", seen.Get("Content-Type"))
	}
}

func TestArchiveRefusesFilesBeyondTheLimitWhileStreaming(t *testing.T) {
	// No declared length: the limit is found while streaming, and the upload
	// is abandoned rather than stored.
	var seen http.Header
	uploaded := false
	upload := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		seen = r.Header.Clone()
		_, err := io.Copy(io.Discard, r.Body)
		uploaded = err == nil
		if err != nil {
			return nil, err
		}
		return response(200, `{"code":0,"data":{}}`), nil
	})}
	_, err := store(context.Background(), "task", 0, "https://provider.example/x", "http://app", "key", source(nil, MaxBytes+1, MaxBytes+1), upload)
	require.ErrorIs(t, err, ErrTooLarge)
	require.False(t, uploaded)
	require.Empty(t, seen.Get("X-Native-Artifact-Type"))
}

func TestArchiveRejectsShortSourcesAndWrongStorageReceipts(t *testing.T) {
	head := []byte("\x89PNG\r\n\x1a\n")
	var seen http.Header
	// Fewer bytes than declared is a failed transfer, retried later.
	_, err := store(context.Background(), "task", 2, "https://provider.example/a.png", "http://app", "key", source(head, 100, 200), application(t, &seen))
	require.ErrorIs(t, err, ErrStorage)
	// Storage that reports other bytes than were sent is never trusted.
	lying := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		n, _ := io.Copy(io.Discard, r.Body)
		raw, _ := json.Marshal(map[string]any{"code": 0, "data": Receipt{Key: "native-results/task/2/" + strings.Repeat("d", 64) + ".bin", SHA256: strings.Repeat("d", 64), Size: n}})
		return response(200, string(raw)), nil
	})}
	_, err = store(context.Background(), "task", 2, "https://provider.example/a.png", "http://app", "key", source(head, 100, 100), lying)
	require.ErrorIs(t, err, ErrStorage)
}

// stalled sends a few bytes and then nothing until its context ends.
type stalled struct {
	sent bool
	done <-chan struct{}
}

func (s *stalled) Read(b []byte) (int, error) {
	if !s.sent {
		s.sent = true
		return copy(b, "\x89PNG\r\n\x1a\n"), nil
	}
	<-s.done
	return 0, errors.New("cancelled")
}

func TestArchiveEndsAStalledTransfer(t *testing.T) {
	previous := stallTimeout
	stallTimeout = 50 * time.Millisecond
	defer func() { stallTimeout = previous }()
	download := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: 100, Body: io.NopCloser(&stalled{done: r.Context().Done()}), Header: make(http.Header)}, nil
	})}
	upload := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		_, err := io.Copy(io.Discard, r.Body)
		return nil, err
	})}
	started := time.Now()
	_, err := store(context.Background(), "task", 0, "https://provider.example/x", "http://app", "key", download, upload)
	require.ErrorIs(t, err, ErrStorage)
	require.Less(t, time.Since(started), 5*time.Second)
}

func TestReceiptValidationCoversTypeAndChunks(t *testing.T) {
	sum := strings.Repeat("a", 64)
	base := Receipt{Key: "native-results/task/0/" + sum + ".bin", SHA256: sum, Size: ChunkSize + 1}
	require.True(t, ValidReceipt("task", 0, base))
	withChunks := base
	withChunks.Type, withChunks.Chunks = "video/mp4", strings.Repeat("b", 128)
	require.True(t, ValidReceipt("task", 0, withChunks))
	for name, mutate := range map[string]func(*Receipt){
		"unknown type":     func(r *Receipt) { r.Type = "text/html" },
		"too few chunks":   func(r *Receipt) { r.Chunks = strings.Repeat("b", 64) },
		"uppercase digest": func(r *Receipt) { r.Chunks = strings.Repeat("B", 128) },
		"over the limit":   func(r *Receipt) { r.Size = MaxBytes + 1; r.Chunks = "" },
	} {
		bad := withChunks
		mutate(&bad)
		require.False(t, ValidReceipt("task", 0, bad), name)
	}
}

func TestMediaTypesComeFromBytesOnly(t *testing.T) {
	for want, head := range map[string]string{
		"image/png":       "\x89PNG\r\n\x1a\n....",
		"image/jpeg":      "\xff\xd8\xff\xe0",
		"image/gif":       "GIF89a",
		"image/webp":      "RIFF\x00\x00\x00\x00WEBPVP8 ",
		"audio/wav":       "RIFF\x00\x00\x00\x00WAVEfmt ",
		"video/mp4":       "\x00\x00\x00\x20ftypmp42",
		"video/quicktime": "\x00\x00\x00\x14ftypqt  ",
		"audio/mp4":       "\x00\x00\x00\x20ftypM4A ",
		"image/avif":      "\x00\x00\x00\x1cftypavif",
		"video/webm":      "\x1a\x45\xdf\xa3\x9fB\x86\x81\x01B\xf7\x81\x01B\x82\x84webm",
		"audio/ogg":       "OggS\x00",
		"audio/flac":      "fLaC\x00",
		"audio/mpeg":      "ID3\x04\x00",
		"audio/aac":       "\xff\xf1\x50\x80",
		"":                "<svg><script>x()</script></svg>",
	} {
		require.Equal(t, want, DetectType([]byte(head)), strconv.Quote(head))
	}
	require.Equal(t, "audio/mpeg", DetectType([]byte{0xff, 0xfb, 0x90, 0x64}))
	require.Equal(t, "application/octet-stream", ContentType("text/html"))
	require.Equal(t, "video/mp4", ContentType("video/mp4"))
	require.Equal(t, "task.mp4", FileName("task", 0, "video/mp4"))
	require.Equal(t, "task-2.png", FileName("task", 2, "image/png"))
	require.Equal(t, "task.bin", FileName("task", 0, ""))
}

func TestUnknownLengthFailsBeforeStorage(t *testing.T) {
	called := false
	upload := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) { called = true; return response(200, ""), nil })}
	_, err := store(context.Background(), "task", 0, "https://provider.example/x", "http://app", "key", source(nil, 12, -1), upload)
	require.ErrorIs(t, err, ErrStorage)
	require.False(t, called)
}
