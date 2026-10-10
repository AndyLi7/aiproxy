package registryvalidation

import (
	"encoding/json"
	"os"
	"testing"
)

func TestRealNullablePromptOutputContracts(t *testing.T) {
	data, err := os.ReadFile("testdata/provider-nullable-prompt.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Endpoint           string
		Contract, Response json.RawMessage
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 6 {
		t.Fatal("missing source fixtures")
	}
	for _, f := range cases {
		t.Run(f.Endpoint, func(t *testing.T) {
			var c struct {
				Providers map[string]struct{ Upstream ProviderSpec }
			}
			if err := json.Unmarshal(f.Contract, &c); err != nil {
				t.Fatal(err)
			}
			p := c.Providers["fal"].Upstream
			frozen, err := FreezeProviderBinding(f.Contract, ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash})
			if err != nil {
				t.Fatal(err)
			}
			var response map[string]any
			if err = json.Unmarshal(f.Response, &response); err != nil {
				t.Fatal(err)
			}
			for _, v := range []any{nil, "", "rewritten prompt"} {
				response["actual_prompt"] = v
				payload, _ := json.Marshal(response)
				if err = ValidateFrozenProviderOutput(frozen, payload); err != nil {
					t.Fatal(v, err)
				}
			}
			for _, v := range []any{1, true, map[string]any{}, []any{"text"}} {
				response["actual_prompt"] = v
				payload, _ := json.Marshal(response)
				if err = ValidateFrozenProviderOutput(frozen, payload); err == nil {
					t.Fatalf("accepted invalid prompt: %v", v)
				}
			}
		})
	}
}
