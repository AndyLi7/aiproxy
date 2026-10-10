package fal

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"net/http"
	"net/http/httptest"
	"reflect"
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

// falVoiceRejection is fal's model error for an unsupported voice. msg, input,
// url and ctx carry free text, the customer's value and supplier identity.
const falVoiceRejection = `{"detail":[{"loc":["body","voice"],"msg":"Voice not found: NoSuchVoice123","type":"feature_not_supported","url":"https://docs.fal.ai/errors#feature_not_supported","input":"NoSuchVoice123","ctx":{"vendor":"elevenlabs"}}]}`

func requireNoFalText(t *testing.T, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"NoSuchVoice123", "Voice not found", "docs.fal.ai", "elevenlabs", "SECRET"} {
		if strings.Contains(string(encoded), leak) {
			t.Fatalf("leaked %q in %s", leak, encoded)
		}
	}
}

// Owner decision 2026-10-09: a submission fal refuses with named fields is an
// input rejection carrying invalid_parameters issues, never fal's text.
func TestNativeSubmitRejectionNamesTheField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		w.Write([]byte(falVoiceRejection))
	}))
	defer server.Close()
	c := &Client{HTTP: server.Client(), BaseURL: server.URL, Key: "test"}
	_, err := c.SubmitNative(context.Background(), "fal-ai/elevenlabs/tts/eleven-v3", []byte(`{"voice":"NoSuchVoice123"}`))
	if !errors.Is(err, adaptor.ErrImageSubmissionRejected) {
		t.Fatalf("want an input rejection, got %v", err)
	}
	var failure *adaptor.ImageSubmissionFailure
	if !errors.As(err, &failure) || failure.PublicError == nil {
		t.Fatalf("missing public error: %v", err)
	}
	if failure.PublicError.Code != "invalid_parameters" || len(failure.PublicError.Issues) != 1 ||
		failure.PublicError.Issues[0] != (model.ImageParameterIssue{Field: "voice", Rule: "unsupported_value"}) {
		t.Fatalf("wrong public error %#v", failure.PublicError)
	}
	if failure.ProviderStatus != 422 || failure.ProviderReason != "feature_not_supported@body.voice" {
		t.Fatalf("wrong evidence %d %q", failure.ProviderStatus, failure.ProviderReason)
	}
	requireNoFalText(t, failure.PublicError)
	requireNoFalText(t, failure.ProviderReason)
}

// fal can accept a request and then refuse its input in the runner: the queue
// says COMPLETED and the result GET answers 400/422 with fal's error body.
func TestNativePollNamesFieldsRejectedAfterAcceptance(t *testing.T) {
	contract, err := nativeresult.CompileTaskContract([]byte(`{"version":1,"model":"m","input_schema":{},"output_schema":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		status     int
		code       string
		issues     []nativeresult.ParameterIssue
		reason     string
	}{
		{"model error 422", falVoiceRejection, 422, "invalid_parameters", []nativeresult.ParameterIssue{{Field: "voice", Rule: "unsupported_value"}}, "feature_not_supported@body.voice"},
		{"model error 400", falVoiceRejection, 400, "invalid_parameters", []nativeresult.ParameterIssue{{Field: "voice", Rule: "unsupported_value"}}, "feature_not_supported@body.voice"},
		{"nested and indexed", `{"detail":[{"type":"one_of","loc":["body","voice_setting","voice_id"],"msg":"SECRET"},{"type":"file_download_error","loc":["body","image_urls",0],"url":"https://docs.fal.ai/x"}]}`, 422, "invalid_parameters", []nativeresult.ParameterIssue{{Field: "voice_setting.voice_id", Rule: "allowed_value"}, {Field: "image_urls[0]", Rule: "file_unreadable"}}, "one_of@body.voice_setting.voice_id, file_download_error@body.image_urls.0"},
		{"unknown type falls back", `{"detail":[{"type":"brand_new_check","loc":["body","speed"],"msg":"SECRET"}]}`, 422, "invalid_parameters", []nativeresult.ParameterIssue{{Field: "speed", Rule: "invalid"}}, "brand_new_check@body.speed"},
		{"flat request error", `{"detail":"Voice not found: NoSuchVoice123","error_type":"request_error"}`, 422, "upstream_result_rejected", nil, "input rejected (details withheld)"},
		{"malformed type", `{"detail":[{"type":"Not A Type","loc":["body","voice"],"msg":"SECRET"}]}`, 422, "upstream_result_rejected", nil, "*@body.voice"},
		{"private field", `{"detail":[{"type":"missing","loc":["body","api_key"]}]}`, 422, "upstream_result_rejected", nil, "missing@body.api_key"},
		{"oversized body", `{"detail":[{"type":"missing","loc":["body","prompt"],"msg":"` + strings.Repeat("a", nativeresult.MaxBytes) + `"}]}`, 422, "upstream_result_rejected", nil, "missing@body.prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/status") {
					w.Write([]byte(`{"status":"COMPLETED"}`))
					return
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer server.Close()
			c := &Client{HTTP: server.Client(), BaseURL: server.URL, Key: "test"}
			result, err := c.PollNative(context.Background(), "fal-ai/elevenlabs/tts/eleven-v3", "request-1", contract)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "failed" || result.ErrorCode != tc.code || !reflect.DeepEqual(result.Issues, tc.issues) {
				t.Fatalf("wrong result %#v", result)
			}
			if result.ProviderStatus != tc.status {
				t.Fatalf("wrong provider status %d", result.ProviderStatus)
			}
			if tc.name != "oversized body" && result.ProviderReason != tc.reason {
				t.Fatalf("wrong provider reason %q", result.ProviderReason)
			}
			requireNoFalText(t, result)
		})
	}
}
