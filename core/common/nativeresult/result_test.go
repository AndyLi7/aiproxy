package nativeresult

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestNativeResultPreservesJSON(t *testing.T) {
	v, err := Compile([]byte(`{"type":"object","required":["seed","masks"],"properties":{"seed":{"type":"integer"},"masks":{"type":"array"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"seed":9007199254740993,"masks":[],"unknown":{"nullable":null,"svg":"<svg></svg>"}}`)
	result, err := v.Validate(raw)
	if err != nil || !bytes.Equal(result, raw) {
		t.Fatalf("lost native output: %s %v", result, err)
	}
	result[0] = ' '
	if raw[0] != '{' {
		t.Fatal("shared result memory")
	}
	for _, bad := range []string{`{"seed":"wrong","masks":[]}`, `{"seed":1}`, `{"seed":1,"seed":2,"masks":[]}`, `{"seed":1,"masks":[]} null`} {
		if _, err := v.Validate([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}
func TestNativeEmptySuccessAndLimits(t *testing.T) {
	v, err := Compile([]byte(`{"anyOf":[{"type":"null"},{"type":"array"},{"type":"object"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`null`, `[]`, `{}`} {
		if _, err := v.Validate([]byte(raw)); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range [][]byte{[]byte(strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66)), []byte(`{"a":{},"a":{}}`), []byte("{\"x\":\"\xff\"}"), []byte(`{"x":"` + strings.Repeat("a", MaxBytes) + `"}`)} {
		if _, err := v.Validate(raw); err == nil {
			t.Fatal("accepted unbounded or ambiguous JSON")
		}
	}
}
func TestNativeSchemaCannotFetchExternalReferences(t *testing.T) {
	if _, err := Compile([]byte(`{"$ref":"https://127.0.0.1/private"}`)); err == nil {
		t.Fatal("accepted external ref")
	}
	v, err := Compile([]byte(`{"$defs":{"value":{"type":"integer"}},"$ref":"#/$defs/value"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate([]byte(`9007199254740993`)); err != nil {
		t.Fatal(err)
	}
}

func TestOfficialNativeResultSchemas(t *testing.T) {
	raw, err := os.ReadFile("testdata/official-results.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Endpoint string
		Schema   json.RawMessage
		Output   json.RawMessage
	}
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Endpoint, func(t *testing.T) {
			v, err := Compile(c.Schema)
			if err != nil {
				t.Fatal("official schema", err)
			}
			got, err := v.Validate(c.Output)
			if err != nil {
				t.Fatal("native result", err)
			}
			if !bytes.Equal(got, c.Output) {
				t.Fatal("result structure or number changed")
			}
		})
	}
}
