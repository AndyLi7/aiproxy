package registryvalidation

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestCompiledIntegerDomainsRemainExact(t *testing.T) {
	data, err := os.ReadFile("testdata/provider-integer-domain.json")
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
	if len(cases) != 7 {
		t.Fatal("missing sources")
	}
	for _, f := range cases {
		t.Run(f.Endpoint, func(t *testing.T) {
			var c struct {
				ID        string `json:"entry_id"`
				Providers map[string]struct{ Upstream ProviderSpec }
			}
			json.Unmarshal(f.Contract, &c)
			p := c.Providers["fal"].Upstream
			b := ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash}
			var body map[string]any
			json.Unmarshal(f.Request, &body)
			field := "upscale_factor"
			if strings.Contains(f.Endpoint, "increase-resolution") {
				field = "desired_increase"
			}
			if strings.Contains(f.Endpoint, "lightning") {
				field = "num_inference_steps"
			}
			if strings.Contains(f.Endpoint, "sam2") {
				body["prompts"] = []any{map[string]any{"label": float64(1), "x": float64(100), "y": float64(100), "frame_index": float64(0)}}
			}
			valid, _ := json.Marshal(body)
			normalized, ve := ValidateImage(f.Contract, c.ID, valid)
			if ve != nil {
				t.Fatal(ve)
			}
			if _, err = MapBoundProviderInput(f.Contract, b, "fal-image", p.Endpoint, "async", normalized); err != nil {
				t.Fatal(err)
			}
			for _, invalid := range []any{1.5, "1", 999.0, true} {
				if strings.Contains(f.Endpoint, "sam2") {
					body["prompts"].([]any)[0].(map[string]any)["label"] = invalid
				} else {
					body[field] = invalid
				}
				bad, _ := json.Marshal(body)
				if _, ve = ValidateImage(f.Contract, c.ID, bad); ve == nil {
					t.Fatalf("public accepted %v", invalid)
				}
				if _, err = MapBoundProviderInput(f.Contract, b, "fal-image", p.Endpoint, "async", bad); err == nil {
					t.Fatalf("provider accepted %v", invalid)
				}
			}
		})
	}
}
