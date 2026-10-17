package nativetask

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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

// chunkedReceipt is what the archive records for data: digests of the whole
// file and of each ChunkSize piece.
func chunkedReceipt(task string, data []byte, mediaType string) ownedartifact.Receipt {
	whole := sha256.Sum256(data)
	chunks := ""
	for start := 0; start < len(data); start += ownedartifact.ChunkSize {
		sum := sha256.Sum256(data[start:min(start+ownedartifact.ChunkSize, len(data))])
		chunks += hex.EncodeToString(sum[:])
	}
	return ownedartifact.Receipt{Key: "native-results/" + task + "/0/" + strings.Repeat("b", 64) + ".bin", SHA256: hex.EncodeToString(whole[:]), Size: int64(len(data)), Type: mediaType, Chunks: chunks}
}

// deliveredVideo stores a completed task whose one artifact is data.
func deliveredVideo(t *testing.T, data []byte) (*Engine, ownedartifact.Receipt) {
	e, plan, _, _ := setup(t)
	ctx := context.Background()
	var c nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(plan.Contract, &c))
	c.Artifacts = []nativeresult.ArtifactBinding{{Path: []string{"file", "url"}}}
	plan.Contract, _ = json.Marshal(c)
	plan.DeliveryBase = "https://gateway.example"
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.NoError(t, model.SaveNativeTaskResult(e.DB, "req", "g", 1, []byte(`{"file":{"url":"https://provider.example/out.mp4"}}`)))
	receipt := chunkedReceipt("req", data, "video/mp4")
	_, err = e.Deliver(ctx, "req", "g", 1, func(context.Context, string, int, string) (ownedartifact.Receipt, error) {
		return receipt, nil
	})
	require.NoError(t, err)
	return e, receipt
}

// mp4Bytes is a file larger than three chunks whose bytes encode their offset.
func mp4Bytes() []byte {
	data := make([]byte, 3*ownedartifact.ChunkSize+12345)
	copy(data, "\x00\x00\x00\x18ftypisom")
	for i := 16; i < len(data); i++ {
		data[i] = byte(i * 7)
	}
	return data
}

func storageServing(t *testing.T, data []byte, key string, calls *[]string) *HTTP {
	return &HTTP{
		Identity: func(*http.Request) (string, int, error) { return "g", 1, nil },
		Download: func(_ *http.Request, k string) (*http.Response, error) {
			require.Equal(t, key, k)
			*calls = append(*calls, "whole")
			return &http.Response{StatusCode: 200, ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data))}, nil
		},
		DownloadRange: func(_ *http.Request, k string, from, to int64) (*http.Response, error) {
			require.Equal(t, key, k)
			*calls = append(*calls, fmt.Sprintf("%d-%d", from, to))
			return &http.Response{StatusCode: 206, ContentLength: to - from + 1, Body: io.NopCloser(bytes.NewReader(data[from : to+1])), Header: http.Header{"Content-Range": []string{fmt.Sprintf("bytes %d-%d/%d", from, to, len(data))}}}, nil
		},
	}
}

func getArtifact(h *HTTP, rangeHeader string, extra ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/v1/model-tasks/req/artifacts/0", nil)
	if rangeHeader != "" {
		r.Header.Set("Range", rangeHeader)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		r.Header.Set(extra[i], extra[i+1])
	}
	w := httptest.NewRecorder()
	h.GetArtifact(w, r, "req", "0")
	return w
}

func TestArtifactDownloadNamesTheFileAndServesVerifiedRanges(t *testing.T) {
	data := mp4Bytes()
	e, receipt := deliveredVideo(t, data)
	var calls []string
	h := storageServing(t, data, receipt.Key, &calls)
	h.Engine = e
	size := int64(len(data))
	chunk := int64(ownedartifact.ChunkSize)

	whole := getArtifact(h, "")
	require.Equal(t, 200, whole.Code)
	require.Equal(t, data, whole.Body.Bytes())
	require.Equal(t, "video/mp4", whole.Header().Get("Content-Type"))
	require.Equal(t, `attachment; filename="req.mp4"`, whole.Header().Get("Content-Disposition"))
	require.Equal(t, "bytes", whole.Header().Get("Accept-Ranges"))
	require.Equal(t, "nosniff", whole.Header().Get("X-Content-Type-Options"))
	require.Contains(t, whole.Header().Get("Content-Security-Policy"), "sandbox")
	require.Equal(t, []string{"whole"}, calls)

	// A range across a chunk boundary reads only the two chunks it touches.
	calls = nil
	from, to := chunk-10, chunk+20
	part := getArtifact(h, fmt.Sprintf("bytes=%d-%d", from, to))
	require.Equal(t, 206, part.Code)
	require.Equal(t, data[from:to+1], part.Body.Bytes())
	require.Equal(t, fmt.Sprintf("bytes %d-%d/%d", from, to, size), part.Header().Get("Content-Range"))
	require.Equal(t, fmt.Sprint(to-from+1), part.Header().Get("Content-Length"))
	require.Equal(t, []string{fmt.Sprintf("0-%d", 2*chunk-1)}, calls)

	// Open-ended and suffix ranges end at the last byte; the last chunk is short.
	calls = nil
	open := getArtifact(h, fmt.Sprintf("bytes=%d-", size-100))
	require.Equal(t, 206, open.Code)
	require.Equal(t, data[size-100:], open.Body.Bytes())
	require.Equal(t, []string{fmt.Sprintf("%d-%d", 3*chunk, size-1)}, calls)
	suffix := getArtifact(h, "bytes=-5")
	require.Equal(t, 206, suffix.Code)
	require.Equal(t, data[size-5:], suffix.Body.Bytes())
	beyond := getArtifact(h, fmt.Sprintf("bytes=10-%d", size+999))
	require.Equal(t, 206, beyond.Code)
	require.Equal(t, data[10:], beyond.Body.Bytes())

	// A range starting past the end is refused with the file's size.
	refused := getArtifact(h, fmt.Sprintf("bytes=%d-", size))
	require.Equal(t, 416, refused.Code)
	require.Equal(t, fmt.Sprintf("bytes */%d", size), refused.Header().Get("Content-Range"))
	require.Contains(t, refused.Body.String(), "range_not_satisfiable")

	// Several ranges, If-Range and malformed values are answered with the whole file.
	for _, header := range []string{"bytes=0-1,5-6", "bytes=abc", "items=0-1", "bytes=9-3"} {
		require.Equal(t, 200, getArtifact(h, header).Code, header)
	}
	require.Equal(t, 200, getArtifact(h, "bytes=0-1", "If-Range", `"etag"`).Code)
}

func TestArtifactRangeNeverSendsUnverifiedBytes(t *testing.T) {
	data := mp4Bytes()
	e, receipt := deliveredVideo(t, data)
	stored := append([]byte(nil), data...)
	h := storageServing(t, stored, receipt.Key, new([]string))
	h.Engine = e
	chunk := ownedartifact.ChunkSize

	// A corrupted first chunk fails before any header or byte.
	stored[5] ^= 0xff
	first := getArtifact(h, "")
	require.Equal(t, 503, first.Code)
	require.Contains(t, first.Body.String(), "artifact_integrity_failed")
	stored[5] ^= 0xff

	// A corrupted later chunk ends the response at the last verified chunk.
	stored[2*chunk+1] ^= 0xff
	later := getArtifact(h, "")
	require.Equal(t, 200, later.Code)
	require.Equal(t, data[:2*chunk], later.Body.Bytes())
	require.Equal(t, fmt.Sprint(len(data)), later.Header().Get("Content-Length"))
	part := getArtifact(h, fmt.Sprintf("bytes=%d-%d", chunk, 2*chunk+5))
	require.Equal(t, 206, part.Code)
	require.Equal(t, data[chunk:2*chunk], part.Body.Bytes())
}

func TestArtifactArchivedBeforeChunksIgnoresRanges(t *testing.T) {
	e, plan, _, _ := setup(t)
	ctx := context.Background()
	var c nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(plan.Contract, &c))
	c.Artifacts = []nativeresult.ArtifactBinding{{Path: []string{"file", "url"}}}
	plan.Contract, _ = json.Marshal(c)
	plan.DeliveryBase = "https://gateway.example"
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.NoError(t, model.SaveNativeTaskResult(e.DB, "req", "g", 1, []byte(`{"file":{"url":"https://provider.example/a.png"}}`)))
	data := []byte("\x89PNG\r\n\x1a\nlegacy")
	sum := sha256.Sum256(data)
	legacy := ownedartifact.Receipt{Key: "native-results/req/0/" + strings.Repeat("c", 64) + ".bin", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
	_, err = e.Deliver(ctx, "req", "g", 1, func(context.Context, string, int, string) (ownedartifact.Receipt, error) { return legacy, nil })
	require.NoError(t, err)
	h := storageServing(t, data, legacy.Key, new([]string))
	h.Engine = e
	w := getArtifact(h, "bytes=0-3")
	require.Equal(t, 200, w.Code)
	require.Equal(t, data, w.Body.Bytes())
	require.Empty(t, w.Header().Get("Accept-Ranges"))
	// No recorded type: still served as an opaque download.
	require.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
	require.Equal(t, `attachment; filename="req.bin"`, w.Header().Get("Content-Disposition"))
}

func TestDeliveredOutputNamesFilesAndDropsProviderAddresses(t *testing.T) {
	e, plan, _, _ := setup(t)
	ctx := context.Background()
	var c nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(plan.Contract, &c))
	c.Artifacts = []nativeresult.ArtifactBinding{{Path: []string{"video", "url"}}}
	plan.Contract, _ = json.Marshal(c)
	plan.DeliveryBase = "https://gateway.example"
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	output := `{"video":{"url":"https://v3.fal.media/files/out.mp4","file_name":"fal_out.mp4","content_type":"application/octet-stream"},"thumbnail_url":"https://v3.fal.media/files/thumb.jpg","seed":7}`
	require.NoError(t, model.SaveNativeTaskResult(e.DB, "req", "g", 1, []byte(output)))
	receipt := chunkedReceipt("req", mp4Bytes(), "video/mp4")
	task, err := e.Deliver(ctx, "req", "g", 1, func(context.Context, string, int, string) (ownedartifact.Receipt, error) { return receipt, nil })
	require.NoError(t, err)
	var delivered map[string]any
	require.NoError(t, json.Unmarshal([]byte(task.DeliveredOutput), &delivered))
	video := delivered["video"].(map[string]any)
	require.Equal(t, "https://gateway.example/v1/model-tasks/req/artifacts/0", video["url"])
	require.Equal(t, "req.mp4", video["file_name"])
	require.Equal(t, "video/mp4", video["content_type"])
	require.NotContains(t, delivered, "thumbnail_url")
	require.NotContains(t, task.DeliveredOutput, "fal")
	require.Equal(t, json.Number("7"), json.Number(fmt.Sprint(delivered["seed"])))
}

func TestArchivingExtendsRecoveryLease(t *testing.T) {
	e, plan, _, _ := setup(t)
	ctx := context.Background()
	var c nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(plan.Contract, &c))
	c.Artifacts = []nativeresult.ArtifactBinding{{Path: []string{"file", "url"}}}
	plan.Contract, _ = json.Marshal(c)
	plan.DeliveryBase = "https://gateway.example"
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.NoError(t, model.SaveNativeTaskResult(e.DB, "req", "g", 1, []byte(`{"file":{"url":"https://provider.example/out.mp4"}}`)))
	require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "req").Update("next_recovery_at", 0).Error)
	receipt := chunkedReceipt("req", mp4Bytes(), "video/mp4")
	archived := false
	_, err = e.RecoverOnce(ctx, "worker", time.Now(), nil, func(archiveCtx context.Context, task string, index int, source string) (ownedartifact.Receipt, error) {
		var row model.NativeTask
		require.NoError(t, e.DB.Where("id = ?", "req").First(&row).Error)
		// The lease covers the archive budget, not only the 2-minute claim.
		require.Len(t, row.RecoveryOwner, 64)
		require.NotEqual(t, "worker", row.RecoveryOwner)
		require.Greater(t, row.RecoveryUntil, time.Now().Add(archiveBudget).Unix())
		deadline, ok := archiveCtx.Deadline()
		require.True(t, ok)
		require.Greater(t, time.Until(deadline), 9*time.Minute)
		archived = true
		return receipt, nil
	})
	require.NoError(t, err)
	require.True(t, archived)
}

func TestRequiredProviderAddressIsBlankedNotRemoved(t *testing.T) {
	e, plan, _, _ := setup(t)
	ctx := context.Background()
	var c nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(plan.Contract, &c))
	c.Artifacts = []nativeresult.ArtifactBinding{{Path: []string{"file", "url"}}}
	c.OutputSchema = json.RawMessage(`{"type":"object","required":["file","preview"],"properties":{"preview":{"type":"string"}}}`)
	plan.Contract, _ = json.Marshal(c)
	plan.DeliveryBase = "https://gateway.example"
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.NoError(t, model.SaveNativeTaskResult(e.DB, "req", "g", 1, []byte(`{"file":{"url":"https://provider.example/a.png"},"preview":"https://provider.example/p.jpg"}`)))
	receipt := chunkedReceipt("req", []byte("\x89PNG\r\n\x1a\nimage"), "image/png")
	task, err := e.Deliver(ctx, "req", "g", 1, func(context.Context, string, int, string) (ownedartifact.Receipt, error) { return receipt, nil })
	require.NoError(t, err)
	require.Contains(t, task.DeliveredOutput, `"preview":""`)
	require.NotContains(t, task.DeliveredOutput, "provider.example")
}

func TestArchiveLosingLeaseCannotCheckpointOrConfirmAndCanResume(t *testing.T) {
	e, plan, wallet, provider := setup(t)
	var contract nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(plan.Contract, &contract))
	contract.Artifacts = []nativeresult.ArtifactBinding{{Path: []string{"file", "url"}}}
	plan.Contract, _ = json.Marshal(contract)
	plan.DeliveryBase = "https://gateway.example"
	_, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.NoError(t, model.SaveNativeTaskResult(e.DB, "req", "g", 1, []byte(`{"file":{"url":"https://provider.example/a.png"}}`)))
	receipt := chunkedReceipt("req", []byte("ID3 audio"), "audio/mpeg")
	_, err = e.Deliver(context.Background(), "req", "g", 1, func(ctx context.Context, _ string, _ int, _ string) (ownedartifact.Receipt, error) {
		// While the first caller owns the lease a second direct caller cannot archive.
		_, otherErr := e.Deliver(context.Background(), "req", "g", 1, func(context.Context, string, int, string) (ownedartifact.Receipt, error) {
			t.Fatal("concurrent archive")
			return receipt, nil
		})
		require.ErrorIs(t, otherErr, model.ErrNativeTaskConflict)
		require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "req").Updates(map[string]any{"recovery_owner": "replacement", "recovery_until": time.Now().Add(time.Minute).Unix()}).Error)
		return receipt, nil
	})
	require.ErrorIs(t, err, model.ErrNativeTaskConflict)
	row, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "result_received", row.Status)
	require.Empty(t, row.ArtifactManifest)
	require.Empty(t, row.DeliveredOutput)
	require.False(t, refundedWith(wallet, "platform_failure"))
	require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "req").Update("recovery_until", 0).Error)
	resumed, err := e.Deliver(context.Background(), "req", "g", 1, func(context.Context, string, int, string) (ownedartifact.Receipt, error) { return receipt, nil })
	require.NoError(t, err)
	require.Equal(t, "completed", resumed.Status)
	require.Equal(t, 1, provider.calls)
}

func TestArchiveRespectsCallerCancellation(t *testing.T) {
	e, plan, _, _ := setup(t)
	var contract nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(plan.Contract, &contract))
	contract.Artifacts = []nativeresult.ArtifactBinding{{Path: []string{"file", "url"}}}
	plan.Contract, _ = json.Marshal(contract)
	plan.DeliveryBase = "https://gateway.example"
	_, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.NoError(t, model.SaveNativeTaskResult(e.DB, "req", "g", 1, []byte(`{"file":{"url":"https://provider.example/a"}}`)))
	ctx, cancel := context.WithCancel(context.Background())
	_, err = e.Deliver(ctx, "req", "g", 1, func(ctx context.Context, _ string, _ int, _ string) (ownedartifact.Receipt, error) {
		cancel()
		<-ctx.Done()
		return chunkedReceipt("req", []byte("x"), ""), nil
	})
	require.ErrorIs(t, err, context.Canceled)
	row, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Empty(t, row.ArtifactManifest)
	require.Equal(t, "result_received", row.Status)
}

func TestArtifactRangeRejectsLyingStorageHeadersAndExtraBytes(t *testing.T) {
	data := mp4Bytes()
	e, receipt := deliveredVideo(t, data)
	h := storageServing(t, data, receipt.Key, new([]string))
	h.Engine = e
	for _, header := range []string{"bytes 0-2097151/1", "bytes 1-2097151/6303801", "bytes 0-2097151/999999999999"} {
		h.DownloadRange = func(r *http.Request, key string, from, to int64) (*http.Response, error) {
			return &http.Response{StatusCode: 206, ContentLength: to - from + 1, Header: http.Header{"Content-Range": []string{header}}, Body: io.NopCloser(bytes.NewReader(data[from : to+1]))}, nil
		}
		require.Equal(t, 503, getArtifact(h, "bytes=0-1").Code)
	}
	h.Download = func(*http.Request, string) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: int64(len(data)), Body: io.NopCloser(io.MultiReader(bytes.NewReader(data), strings.NewReader("extra")))}, nil
	}
	w := getArtifact(h, "")
	require.Less(t, w.Body.Len(), len(data))
}

func TestUnsanitizableProviderURLFailsClosedAndRefunds(t *testing.T) {
	e, plan, wallet, _ := setup(t)
	var contract nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(plan.Contract, &contract))
	contract.OutputSchema = json.RawMessage(`{"type":"object","required":["preview"],"properties":{"preview":{"type":"string","minLength":1}}}`)
	plan.Contract, _ = json.Marshal(contract)
	_, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.NoError(t, model.SaveNativeTaskResult(e.DB, "req", "g", 1, []byte(`{"preview":"https://fal.media/private?signature=secret"}`)))
	failed, err := e.Deliver(context.Background(), "req", "g", 1, nil)
	require.NoError(t, err)
	require.Equal(t, "failed", failed.Status)
	require.Empty(t, failed.DeliveredOutput)
	require.True(t, refundedWith(wallet, "platform_failure"))
}
