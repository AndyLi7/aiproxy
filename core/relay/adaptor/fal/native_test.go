package fal

import (
	"context"
	"encoding/json"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativePollPreservesOutputAndFullSubmitPath(t *testing.T) {
	outputs := []string{`{"file":{"url":"https://example.org/a.zip"},"image":null}`, `{"svg_content":"<svg/>","seed":9007199254740993}`, `{"results":{"quad_boxes":[]}}`, `null`}
	for _, output := range outputs {
		t.Run(output[:4], func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Key test" {
					t.Error("missing auth")
				}
				switch r.URL.Path {
				case "/fal-ai/deep/model/variant":
					if r.Method != "POST" {
						t.Error("method")
					}
					posts++
					w.Write([]byte(`{"request_id":"request-1"}`))
				case "/fal-ai/deep/requests/request-1/status":
					w.Write([]byte(`{"status":"COMPLETED"}`))
				case "/fal-ai/deep/requests/request-1":
					w.Write([]byte(output))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			c := &Client{BaseURL: server.URL, Key: "test"}
			contract, err := nativeresult.CompileTaskContract([]byte(`{"version":1,"model":"test/native","input_schema":{"type":"object"},"output_schema":{}}`))
			if err != nil {
				t.Fatal(err)
			}
			id, err := c.SubmitNative(context.Background(), "fal-ai/deep/model/variant", []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			result, err := c.PollNative(context.Background(), "fal-ai/deep/model/variant", id, contract)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "result_received" || string(result.Output) != output || posts != 1 {
				t.Fatalf("changed output %s %#v", result.Output, result)
			}
		})
	}
}
func TestNativePollRejectsMalformedResultsAndRedirects(t *testing.T) {
	for _, body := range []string{`{} {}`, `{"x":1,"x":2}`, `{"x":"` + strings.Repeat("a", nativeresult.MaxBytes) + `"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/status") {
				w.Write([]byte(`{"status":"COMPLETED"}`))
				return
			}
			w.Write([]byte(body))
		}))
		c := &Client{BaseURL: server.URL}
		contract, _ := nativeresult.CompileTaskContract([]byte(`{"version":1,"model":"m","input_schema":{},"output_schema":{}}`))
		if _, err := c.PollNative(context.Background(), "fal-ai/m", "id", contract); err == nil {
			t.Fatal("accepted malformed result")
		}
		server.Close()
	}
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer server.Close()
	c := &Client{BaseURL: server.URL, Key: "private"}
	var raw json.RawMessage
	_, _, err := c.nativeRequest(context.Background(), http.MethodGet, "model/requests/id", raw)
	if err == nil || redirected {
		t.Fatal("followed credential redirect")
	}
}
