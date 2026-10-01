package registryvalidation

import (
	"encoding/json"
	"os"
	"testing"
)

func TestBoundEmptyPromptDefault(t *testing.T) {
	for _, tc := range []struct {
		name, provider, field, body string
		ok                          bool
	}{
		{"bound empty default", `"provider_contract_version":1,`, `"default":""`, `"prompt":""`, true},
		{"bound omitted default", `"provider_contract_version":1,`, `"default":""`, `"n":1`, true},
		{"legacy still guarded", "", `"default":""`, `"prompt":""`, false},
		{"bound without explicit default", `"provider_contract_version":1,`, `"description":"prompt"`, `"prompt":""`, false},
		{"whitespace is not empty sentinel", `"provider_contract_version":1,`, `"default":""`, `"prompt":" "`, false},
		{"schema min length still enforced", `"provider_contract_version":1,`, `"default":"","minLength":1`, `"prompt":""`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := []byte(`{` + tc.provider + `"entry_id":"test/edit","validation_version":1,"input_schema":{"type":"object","properties":{"prompt":{"type":"string",` + tc.field + `},"n":{"type":"integer"}},"required":["prompt"],"additionalProperties":false}}`)
			_, err := ValidateImage(c, "test/edit", []byte(`{"model":"test/edit",`+tc.body+`}`))
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected validation: %v", err)
			}
		})
	}
}

func TestRealSourceEmptyPromptDefaults(t *testing.T) {
	data, err := os.ReadFile("testdata/provider-empty-prompt.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Endpoint string
		Contract json.RawMessage
		Request  json.RawMessage
	}
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 7 {
		t.Fatal("missing source cases")
	}
	for _, f := range fixtures {
		t.Run(f.Endpoint, func(t *testing.T) {
			var c struct {
				ID        string `json:"entry_id"`
				Providers map[string]struct {
					Upstream ProviderSpec `json:"upstream"`
				}
			}
			if err := json.Unmarshal(f.Contract, &c); err != nil {
				t.Fatal(err)
			}
			normalized, ve := ValidateImage(f.Contract, c.ID, f.Request)
			if ve != nil {
				t.Fatal(ve)
			}
			p := c.Providers["fal"].Upstream
			b := ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash}
			mapped, err := MapBoundProviderInput(f.Contract, b, "fal-image", p.Endpoint, "async", normalized)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = ResolveImageMetering(f.Contract, b, mapped, 1, 1024, true); err != nil {
				t.Fatal(err)
			}
		})
	}
}
