package registryvalidation

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestRealStylePresetsAndUniqueItems(t *testing.T) {
	data, err := os.ReadFile("testdata/provider-style-presets.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Endpoint          string
		Contract, Request json.RawMessage
	}
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 8 {
		t.Fatal("missing source fixtures")
	}
	for _, f := range fixtures {
		t.Run(f.Endpoint, func(t *testing.T) {
			var c struct {
				ID        string `json:"entry_id"`
				Providers map[string]struct{ Upstream ProviderSpec }
			}
			if err = json.Unmarshal(f.Contract, &c); err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			json.Unmarshal(f.Request, &body)
			key := "style_preset"
			value := any("FLAT_VECTOR")
			if strings.Contains(f.Endpoint, "fooocus") {
				key = "styles"
				value = []any{"Simple Vector Art"}
			}
			body[key] = value
			request, _ := json.Marshal(body)
			normalized, ve := ValidateImage(f.Contract, c.ID, request)
			if ve != nil {
				t.Fatal(ve)
			}
			p := c.Providers["fal"].Upstream
			b := ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash}
			if _, err = MapBoundProviderInput(f.Contract, b, "fal-image", p.Endpoint, "async", normalized); err != nil {
				t.Fatal(err)
			}
			if key == "styles" {
				body[key] = []any{"Simple Vector Art", "Simple Vector Art"}
				bad, _ := json.Marshal(body)
				if _, ve = ValidateImage(f.Contract, c.ID, bad); ve == nil {
					t.Fatal("public duplicate accepted")
				}
				if _, err = MapBoundProviderInput(f.Contract, b, "fal-image", p.Endpoint, "async", bad); err == nil {
					t.Fatal("provider duplicate accepted")
				}
			}
		})
	}
}
