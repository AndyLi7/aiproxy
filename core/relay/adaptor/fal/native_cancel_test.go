package fal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fal queue cancel: PUT {owner/app}/requests/{id}/cancel answers 202
// CANCELLATION_REQUESTED, 400 ALREADY_COMPLETED or 404 NOT_FOUND. Each is an
// outcome to log, not an error; anything else is an error. One request only.
func TestNativeCancelUsesQueueCancelEndpoint(t *testing.T) {
	cases := []struct {
		status  int
		outcome string
		wantErr bool
	}{
		{http.StatusAccepted, NativeCancelRequested, false},
		{http.StatusOK, NativeCancelRequested, false},
		{http.StatusBadRequest, NativeCancelAlreadyCompleted, false},
		{http.StatusNotFound, NativeCancelNotFound, false},
		{http.StatusInternalServerError, "", true},
		{http.StatusFound, "", true},
	}
	for _, tc := range cases {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			raw, _ := io.ReadAll(r.Body)
			if r.Method != http.MethodPut || r.URL.Path != "/fal-ai/deep/requests/request-1/cancel" || r.Header.Get("Authorization") != "Key test" || len(raw) != 0 {
				t.Errorf("unexpected %s %s body=%q", r.Method, r.URL.Path, raw)
			}
			if tc.status == http.StatusFound {
				w.Header().Set("Location", "/elsewhere")
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"status":"CANCELLATION_REQUESTED"}`))
		}))
		outcome, err := (&Client{BaseURL: server.URL, Key: "test"}).CancelNative(context.Background(), "fal-ai/deep/model/variant", "request-1")
		server.Close()
		if (err != nil) != tc.wantErr || outcome != tc.outcome || calls != 1 {
			t.Fatalf("status %d: outcome=%q err=%v calls=%d", tc.status, outcome, err, calls)
		}
	}
}

func TestNativeCancelRejectsUnsafeBindingsWithoutARequest(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Key: "test"}
	for _, binding := range [][2]string{{"fal-ai/deep", "../x"}, {"fal-ai/deep", ""}, {"fal-ai/deep", "a/b"}, {"../deep", "request-1"}, {"single", "request-1"}} {
		if _, err := client.CancelNative(context.Background(), binding[0], binding[1]); err == nil {
			t.Fatalf("unsafe binding %v accepted", binding)
		}
	}
	if calls != 0 {
		t.Fatalf("unsafe bindings sent %d requests", calls)
	}
}
