package registryvalidation

import (
	"encoding/json"
	"os"
	"testing"
)

func TestPlatformAliasesCannotBeOverridden(t *testing.T) {
	data, err := os.ReadFile("testdata/provider-platform-aliases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Endpoint          string
		Contract, Request json.RawMessage
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 {
		t.Fatal("missing source fixtures")
	}
	for _, f := range cases {
		t.Run(f.Endpoint, func(t *testing.T) {
			var c struct {
				ID        string `json:"entry_id"`
				Providers map[string]struct{ Upstream ProviderSpec }
			}
			if err := json.Unmarshal(f.Contract, &c); err != nil {
				t.Fatal(err)
			}
			p := c.Providers["fal"].Upstream
			b := ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash}
			var body map[string]any
			if err := json.Unmarshal(f.Request, &body); err != nil {
				t.Fatal(err)
			}
			field := "enable_safety_checks"
			value := true
			if f.Endpoint == "bytedance/seedream/v5/lite/text-to-image" {
				field = "return_byteplus_urls"
				value = false
			}
			values := []any{value}
			for _, value := range values {
				delete(body, field)
				payload, _ := json.Marshal(body)
				normalized, ve := ValidateImage(f.Contract, c.ID, payload)
				if ve != nil {
					t.Fatal(ve)
				}
				mapped, err := MapBoundProviderInput(f.Contract, b, "fal-image", p.Endpoint, "async", normalized)
				if err != nil {
					t.Fatal(err)
				}
				var sent map[string]any
				if err = json.Unmarshal(mapped, &sent); err != nil {
					t.Fatal(err)
				}
				if sent[field] != value {
					t.Fatalf("native parameter changed: %v != %v", sent[field], value)
				}
			}
			body[field] = !value
			payload, _ := json.Marshal(body)
			if _, ve := ValidateImage(f.Contract, c.ID, payload); ve == nil {
				t.Fatal("wrong parameter type accepted")
			}
		})
	}
}
