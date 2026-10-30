//nolint:testpackage // Reuses the native execution fixture.
package nativetask

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestImageWaitDoesNotChangeBillingOrDispatch(t *testing.T) {
	e, plan, w, p := setup(t)
	h := &HTTP{
		Engine:      e,
		Identity:    func(*http.Request) (string, int, error) { return "g", 1, nil },
		ResolvePlan: func(*http.Request, []byte) (Plan, Provider, error) { return plan, p, nil },
	}
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/images/generations",
		nil,
	)
	task := h.CreateTask(httptest.NewRecorder(), req, "compat", body)
	require.NotNil(t, task)

	count := len(w.commands)
	waited, err := h.WaitImage(context.Background(), task, time.Now().Add(5*time.Millisecond))
	require.NoError(t, err)
	require.Equal(t, task.Status, waited.Status)
	require.Equal(t, count, len(w.commands))
	require.Equal(t, 1, p.calls)
	// Replays remain owner/fingerprint-checked without a current route resolver.
	h.ResolvePlan = nil
	replay := h.CreateTask(httptest.NewRecorder(), req, "compat", body)
	require.NotNil(t, replay)
	require.Equal(t, count, len(w.commands))
	require.Equal(t, 1, p.calls)

	conflict := httptest.NewRecorder()
	require.Nil(t, h.CreateTask(conflict, req, "compat", []byte(`{}`)))
	require.Equal(t, 409, conflict.Code)

	h.Identity = func(*http.Request) (string, int, error) { return "other", 1, nil }
	require.Nil(t, h.CreateTask(httptest.NewRecorder(), req, "compat", body))
}

func TestOwnedImageDeliveryIntegrityNoFallback(t *testing.T) {
	data := []byte("archived fixture")
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	receipt := ownedartifact.Receipt{
		Key:    "native-results/compat/0/" + digest + ".bin",
		Size:   int64(len(data)),
		SHA256: digest,
		Type:   "image/png",
	}
	raw, err := json.Marshal(map[int]ownedartifact.Receipt{0: receipt})
	require.NoError(t, err)

	task := &model.NativeTask{
		ID:               "compat",
		GroupID:          "g",
		TokenID:          1,
		Status:           "completed",
		DeliveredOutput:  `{"images":[{"url":"owned"}]}`,
		ArtifactManifest: string(raw),
	}
	h := &HTTP{Download: func(*http.Request, string) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: int64(len(data)),
			Body:          io.NopCloser(bytes.NewReader(data)),
		}, nil
	}}
	result, err := h.ReadOwnedImage(
		httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil),
		task,
	)
	require.NoError(t, err)
	require.Equal(t, base64.StdEncoding.EncodeToString(data), result)
	data[0] = 'X'
	_, err = h.ReadOwnedImage(
		httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil),
		task,
	)
	require.Error(t, err)

	task.DeliveredOutput = `{"images":[{},{}]}`
	_, ok := SingleOwnedImage(task)
	require.False(t, ok)
}

func TestImageErrorInstructionsUseAdapterIdentity(t *testing.T) {
	h := &HTTP{ErrorIdentityHeader: "Idempotency-Key"}
	result := httptest.NewRecorder()
	h.writeError(result, 503, "model_route_unavailable")
	require.Contains(t, result.Body.String(), "Idempotency-Key")
	require.NotContains(t, result.Body.String(), "X-Request-Id")

	h.ErrorIdentityHeader = ""
	result = httptest.NewRecorder()
	h.writeError(result, 503, "model_route_unavailable")
	require.Contains(t, result.Body.String(), "X-Request-Id")
}
