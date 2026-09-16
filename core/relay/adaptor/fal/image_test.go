package fal

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQueueLifecycle(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Key secret", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/fal-ai/minimax/image-01":
			calls++
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, float64(1), body["num_images"])
			require.NotContains(t, body, "n")
			require.NotContains(t, body, "model")
			w.Write([]byte(`{"request_id":"abc"}`))
		case "/fal-ai/minimax/requests/abc/status":
			w.Write([]byte(`{"status":"COMPLETED"}`))
		case "/fal-ai/minimax/requests/abc":
			w.Write([]byte(`{"images":[{"url":"https://cdn.example/image.png","content_type":"image/png"}]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client := Client{HTTP: server.Client(), BaseURL: server.URL, Key: "secret"}
	id, err := client.Submit(t.Context(), "fal-ai/minimax/image-01", []byte(`{"prompt":"test","num_images":1}`))
	require.NoError(t, err)
	require.Equal(t, "abc", id)
	result, err := client.Poll(t.Context(), "fal-ai/minimax/image-01", id)
	require.NoError(t, err)
	require.Equal(t, "completed", result.Status)
	require.Len(t, result.Data, 1)
	require.Equal(t, 1, calls)
}

func TestRejectInvalidSuccess(t *testing.T) {
	for _, body := range []string{`{"images":[]}`, `{"images":[{"url":"http://cdn.example/a"}]}`, `{"images":[{"url":"https://user:pass@cdn.example/a"}]}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path[len(r.URL.Path)-6:] == "status" {
					w.Write([]byte(`{"status":"COMPLETED"}`))
				} else {
					w.Write([]byte(body))
				}
			}))
			defer s.Close()
			c := Client{HTTP: s.Client(), BaseURL: s.URL}
			result, err := c.Poll(t.Context(), "fal-ai/minimax/image-01", "abc")
			require.NoError(t, err)
			require.Equal(t, "failed", result.Status)
			require.Empty(t, result.Data)
		})
	}
}

func TestFalRejectsSynchronousRelay(t *testing.T) {
	a := &Adaptor{}
	_, err := a.GetRequestURL(nil, nil, nil)
	require.Error(t, err)
}
func TestFalPollingErrorsAndNoCredentialRedirect(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		code       int
		want       string
	}{
		{"queued", `{"status":"IN_QUEUE"}`, 200, "queued"},
		{"running", `{"status":"IN_PROGRESS"}`, 200, "in_progress"},
		{"terminal", `{"status":"COMPLETED","error":"private upstream details"}`, 200, "failed"},
		{"transient", `{}`, 503, ""},
		{"redirect", `{}`, 307, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "http://invalid.example/leak")
				w.WriteHeader(tc.code)
				w.Write([]byte(tc.body))
			}))
			defer s.Close()
			c := Client{HTTP: s.Client(), BaseURL: s.URL, Key: "secret"}
			result, err := c.Poll(t.Context(), "fal-ai/minimax/image-01", "abc")
			if tc.want == "" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, result.Status)
			}
			require.Equal(t, 1, calls)
		})
	}
}

func TestSubmissionRejectionIsDistinctFromUnknownAcceptance(t *testing.T) {
	for _, code := range []int{400, 401, 422, 429, 500, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
			defer s.Close()
			c := Client{HTTP: s.Client(), BaseURL: s.URL}
			_, err := c.Submit(t.Context(), "fal-ai/minimax/image-01", []byte(`{"prompt":"test","n":1}`))
			require.Error(t, err)
			require.Equal(t, code < 500, errors.Is(err, adaptor.ErrImageSubmissionRejected))
		})
	}
}
