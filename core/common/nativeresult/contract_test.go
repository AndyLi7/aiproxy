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
