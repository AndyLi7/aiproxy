package nativeresult

import (
	"bytes"
	"testing"
)

func TestNativeTaskEnvelopeBindsModelAndPreservesInput(t *testing.T) {
	c, err := CompileTaskContract([]byte(`{"version":1,"model":"brand/model/recognition","input_schema":{"type":"object","required":["seed"],"properties":{"seed":{"type":"integer"},"prompt":{"type":"string","default":"upstream default"}},"additionalProperties":false},"output_schema":{"type":"object"}}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.ValidateRequest([]byte(`{"model":"brand/model/recognition","input":{"seed":9007199254740993}}`))
	if err != nil || !bytes.Equal(got, []byte(`{"seed":9007199254740993}`)) {
		t.Fatalf("coerced input %s: %v", got, err)
	}
	for _, raw := range []string{`{"model":"different","input":{"seed":1}}`, `{"model":"brand/model/recognition"}`, `{"model":"brand/model/recognition","provider":"fal","input":{"seed":1}}`, `{"model":"brand/model/recognition","input":{"seed":1,"url":"https://private"}}`, `{"model":"brand/model/recognition","input":{"seed":1,"seed":2}}`} {
		if _, err := c.ValidateRequest([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestNativePlatformControlsAreServerOwned(t *testing.T) {
	c, err := CompileTaskContract([]byte(`{"version":1,"model":"native","input_schema":{"type":"object","properties":{"seed":{"type":"integer"}},"required":["seed"],"additionalProperties":false},"upstream_input_schema":{"type":"object","properties":{"seed":{"type":"integer"},"enable_safety_checker":{"const":true},"sync_mode":{"const":false}},"required":["seed","enable_safety_checker","sync_mode"],"additionalProperties":false},"fixed_parameters":{"enable_safety_checker":true,"sync_mode":false},"output_schema":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	input, err := c.ValidateRequest([]byte(`{"model":"native","input":{"seed":9007199254740993}}`))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := c.PrepareUpstreamInput(input)
	if err != nil {
		t.Fatal(err)
	}
	expected := []byte(`{"enable_safety_checker":true,"seed":9007199254740993,"sync_mode":false}`)
	if !bytes.Equal(prepared, expected) {
		t.Fatalf("unexpected projection %s", prepared)
	}
	for _, raw := range []string{`{"seed":1,"sync_mode":true}`, `{"seed":1,"enable_safety_checker":true}`, `{"seed":1,"enable_safety_checker":false}`} {
		if _, err = c.ValidateRequest([]byte(`{"model":"native","input":` + raw + `}`)); err == nil {
			t.Fatal("client override accepted")
		}
		if _, err = c.PrepareUpstreamInput([]byte(raw)); err == nil {
			t.Fatal("direct override accepted")
		}
	}
}

// Issues naming a frozen platform control (by its top-level name) are not the
// customer's; every other field, including look-alike names, is kept.
func TestCustomerIssuesDropPlatformControls(t *testing.T) {
	c, err := CompileTaskContract([]byte(`{"version":1,"model":"native","input_schema":{"type":"object"},"upstream_input_schema":{"type":"object"},"fixed_parameters":{"sync_mode":false,"output_format":"wav"},"output_schema":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	issues := []ParameterIssue{{Field: "sync_mode", Rule: "unsupported_value"}, {Field: "sync_mode_extra", Rule: "boolean"}, {Field: "output_format", Rule: "allowed_value"}, {Field: "image_urls[0]", Rule: "file_size"}, {Field: "voice_setting.voice_id", Rule: "unsupported_value"}}
	got := c.CustomerIssues(issues)
	if len(got) != 3 || got[0].Field != "sync_mode_extra" || got[1].Field != "image_urls[0]" || got[2].Field != "voice_setting.voice_id" {
		t.Fatalf("kept %v", got)
	}
	if got := c.CustomerIssues(issues[:1]); got != nil {
		t.Fatalf("kept a platform control %v", got)
	}
}

// A request made with another accepted ID (public_api_id or an alias) passes
// the same validation with an unchanged input; the contract keeps its model.
func TestNativeTaskEnvelopeAcceptsResolvedPublicID(t *testing.T) {
	c, err := CompileTaskContract([]byte(`{"version":1,"model":"brand/model/text-to-speech","input_schema":{"type":"object","required":["text"],"properties":{"text":{"type":"string"}},"additionalProperties":false},"output_schema":{"type":"object"}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"model":"brand/model","input":{"text":"hi"}}`, `{"model":"brand/model/text-to-speech","input":{"text":"hi"}}`} {
		got, err := c.ValidateRequestAs([]byte(raw), "brand/model")
		if err != nil || !bytes.Equal(got, []byte(`{"text":"hi"}`)) {
			t.Fatalf("%s: %s %v", raw, got, err)
		}
	}
	for _, tc := range []struct{ raw, accepted string }{
		// Without the gateway's resolution the other ID is refused.
		{`{"model":"brand/model","input":{"text":"hi"}}`, ""},
		// Only the one accepted ID, exactly.
		{`{"model":"Brand/Model","input":{"text":"hi"}}`, "brand/model"},
		{`{"model":"other/model","input":{"text":"hi"}}`, "brand/model"},
		// The input is validated as before.
		{`{"model":"brand/model","input":{"text":"hi","extra":1}}`, "brand/model"},
		{`{"model":"brand/model","input":{}}`, "brand/model"},
	} {
		if _, err := c.ValidateRequestAs([]byte(tc.raw), tc.accepted); err == nil {
			t.Fatalf("accepted %s as %q", tc.raw, tc.accepted)
		}
	}
	if _, err := c.ValidateRequest([]byte(`{"model":"brand/model","input":{"text":"hi"}}`)); err == nil {
		t.Fatal("ValidateRequest accepted another ID")
	}
}
