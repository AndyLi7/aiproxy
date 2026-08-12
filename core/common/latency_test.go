package common

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestFormatLatencyEventUsesSafeAllowListedFields(t *testing.T) {
	line, err := FormatLatencyEvent(LatencyEvent{
		Event:      "aiproxy_stage_finished",
		RequestID:  "trace_20260812-aiproxy",
		Stage:      "upstream",
		DurationMS: 123.4567,
		Outcome:    "error",
		Status:     504,
		Method:     "POST",
		Path:       "/v1/videos",
		Model:      "bytedance/seedance-1.0-pro",
		ChannelID:  7,
		ErrorType:  "context_deadline_exceeded",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(line, "\r\n") {
		t.Fatalf("latency event contains a newline: %q", line)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["request_id"] != "trace_20260812-aiproxy" || got["stage"] != "upstream" {
		t.Fatalf("unexpected event: %#v", got)
	}
	if got["duration_ms"] != 123.457 {
		t.Fatalf("duration_ms = %#v, want 123.457", got["duration_ms"])
	}
	for _, forbidden := range []string{"authorization", "cookie", "prompt", "request_body"} {
		if _, ok := got[forbidden]; ok {
			t.Fatalf("forbidden field %q present", forbidden)
		}
	}
}

func TestWriteLatencyEventWritesExactlyOneJSONLine(t *testing.T) {
	var output bytes.Buffer
	err := WriteLatencyEvent(&output, LatencyEvent{
		Event:      "aiproxy_request_finished",
		RequestID:  "trace_20260812-aiproxy",
		DurationMS: 20,
		Outcome:    "cancelled",
		Status:     499,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(output.String(), "\n"); got != 1 {
		t.Fatalf("newline count = %d, want 1: %q", got, output.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &decoded); err != nil {
		t.Fatalf("output is not a standalone JSON line: %v", err)
	}
}
