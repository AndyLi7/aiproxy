package registryvalidation

import (
	"encoding/json"
	"os"
	"testing"
)

func TestCompiledRasterFormatsRemainExact(t *testing.T) {
	data, err := os.ReadFile("testdata/provider-raster-formats.json")
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
	if len(cases) != 8 {
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
			body["output_format"] = "png"
			valid, _ := json.Marshal(body)
			normalized, ve := ValidateImage(f.Contract, c.ID, valid)
			if ve != nil {
				t.Fatal(ve)
			}
			if _, err = MapBoundProviderInput(f.Contract, b, "fal-image", p.Endpoint, "async", normalized); err != nil {
				t.Fatal(err)
			}
			for _, invalid := range []any{"svg", "zip", "gif", "JPEG", 1.0, true} {
				body["output_format"] = invalid
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
