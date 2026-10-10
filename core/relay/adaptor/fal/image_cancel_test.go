package fal_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"github.com/stretchr/testify/require"
)

// frozenFalContract freezes the Seedream 4.5 fixture, optionally re-pointed at
// another endpoint, exactly as admission stores it on an image task.
func frozenFalContract(t *testing.T, endpoint string) (string, []byte) {
	t.Helper()
	raw, err := os.ReadFile("../../../common/registryvalidation/testdata/seedream-4.5-v2-text-to-image.json")
	require.NoError(t, err)
	if endpoint != "" {
		raw = []byte(strings.ReplaceAll(string(raw), "fal-ai/bytedance/seedream/v4.5/text-to-image", endpoint))
	}
	var contract struct {
		Providers map[string]struct {
			Upstream registryvalidation.ProviderSpec `json:"upstream"`
		} `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(raw, &contract))
	spec := contract.Providers["fal"].Upstream
	frozen, err := registryvalidation.FreezeProviderBinding(raw, registryvalidation.ProviderBinding{
		Provider: "fal", ID: spec.ID, Revision: spec.Revision, ContractHash: spec.ContractHash,
	})
	require.NoError(t, err)
	return spec.Endpoint, frozen
}

type cancelRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *cancelRecorder) record(t *testing.T, req *http.Request) {
	t.Helper()
	body, _ := io.ReadAll(req.Body)
	if req.Header.Get("Authorization") != "Key secret" || len(body) != 0 {
		t.Errorf("unexpected cancel auth=%q body=%q", req.Header.Get("Authorization"), body)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, req.Method+" "+req.URL.Path)
}

// An image task's cancel goes to the queue path in its frozen contract, the
// same path its polls read, once and without a body.
func TestImageCancelUsesFrozenQueuePathOnce(t *testing.T) {
	endpoint, frozen := frozenFalContract(t, "")
	rec := &cancelRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(t, r)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"CANCELLATION_REQUESTED"}`))
	}))
	defer server.Close()

	outcome, err := (&fal.Client{BaseURL: server.URL, Key: "secret"}).Cancel(t.Context(), endpoint, "abc", frozen)
	require.NoError(t, err)
	require.Equal(t, fal.NativeCancelRequested, outcome)
	require.Equal(t, []string{"PUT /" + endpoint + "/requests/abc/cancel"}, rec.calls)
}

// Same compatibility rule as Poll (Wan 2.6): an explicit 405 on the full model
// path is retried once at the owner/app root.
func TestImageCancelFallsBackToAppRootAfterMethodRejection(t *testing.T) {
	endpoint, frozen := frozenFalContract(t, "wan/v2.6/text-to-image")
	rec := &cancelRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(t, r)
		if r.URL.Path == "/wan/v2.6/text-to-image/requests/abc/cancel" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer server.Close()

	outcome, err := (&fal.Client{BaseURL: server.URL, Key: "secret"}).Cancel(t.Context(), endpoint, "abc", frozen)
	require.NoError(t, err)
	require.Equal(t, fal.NativeCancelRequested, outcome)
	require.Equal(t, []string{
		"PUT /wan/v2.6/text-to-image/requests/abc/cancel",
		"PUT /wan/v2.6/requests/abc/cancel",
	}, rec.calls)
}

// A 405 at the app root is an error, not a loop; other outcomes keep the
// native mapping (400 already completed, 404 not found).
func TestImageCancelOutcomesAtFrozenPath(t *testing.T) {
	endpoint, frozen := frozenFalContract(t, "")
	for _, tc := range []struct {
		status  int
		outcome string
		fails   bool
	}{
		{http.StatusBadRequest, fal.NativeCancelAlreadyCompleted, false},
		{http.StatusNotFound, fal.NativeCancelNotFound, false},
		{http.StatusInternalServerError, "", true},
		{http.StatusFound, "", true},
	} {
		rec := &cancelRecorder{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec.record(t, r)
			if tc.status == http.StatusFound {
				w.Header().Set("Location", "https://attacker.example/steal")
			}
			w.WriteHeader(tc.status)
		}))
		outcome, err := (&fal.Client{BaseURL: server.URL, Key: "secret"}).Cancel(t.Context(), endpoint, "abc", frozen)
		server.Close()
		require.Equal(t, tc.outcome, outcome, tc.status)
		require.Equal(t, tc.fails, err != nil, tc.status)
		require.Len(t, rec.calls, 1, "cancel is sent once (status %d)", tc.status)
	}

	rec := &cancelRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(t, r)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer server.Close()
	_, err := (&fal.Client{BaseURL: server.URL, Key: "secret"}).Cancel(t.Context(), "fal-ai/minimax/image-01", "abc")
	require.Error(t, err)
	require.Len(t, rec.calls, 1, "the app root itself is never retried")
}

// A contract that does not bind this endpoint, or an unsafe request id, sends
// nothing at all.
func TestImageCancelRefusesUnboundContractsWithoutARequest(t *testing.T) {
	_, frozen := frozenFalContract(t, "")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	client := &fal.Client{BaseURL: server.URL, Key: "secret"}
	_, err := client.Cancel(t.Context(), "fal-ai/other/model", "abc", frozen)
	require.Error(t, err)
	_, err = client.Cancel(t.Context(), "fal-ai/minimax/image-01", "../abc")
	require.Error(t, err)
	_, err = client.Cancel(t.Context(), "fal-ai/minimax/image-01", "abc", frozen, frozen)
	require.Error(t, err)
	require.Zero(t, calls)
}

// The adaptor cancels with the task's own credential at the queue base the
// task was accepted on (AsyncUsageInfo.BaseURL), as PollImage polls.
func TestImageCancelAdaptorUsesTheTaskBinding(t *testing.T) {
	rec := &cancelRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(t, r)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	var canceller adaptor.ImageTaskCanceller = &fal.Adaptor{}
	outcome, err := canceller.CancelImage(
		t.Context(),
		&model.Channel{Type: model.ChannelTypeFal, Key: "secret", BaseURL: "http://127.0.0.1:1"},
		&model.AsyncUsageInfo{BaseURL: server.URL},
		&model.ImageTask{UpstreamModel: "fal-ai/minimax/image-01", UpstreamID: "upstream"},
	)
	require.NoError(t, err)
	require.Equal(t, fal.NativeCancelRequested, outcome)
	require.Equal(t, []string{"PUT /fal-ai/minimax/requests/upstream/cancel"}, rec.calls)
}
