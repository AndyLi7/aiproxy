package registryvalidation

import (
	"encoding/json"
	"os"
	"testing"
)

// Saved-source fixtures exported by the app's current compiler. No provider calls.
func TestFalUpgradeCompiledParameterBehavior(t *testing.T) {
	for _, model := range []string{"nano-banana", "wan-2.2-a14b", "turbo-tiling-lora"} {
		t.Run(model, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/fal-upgrade/" + model + ".json")
			if err != nil {
				t.Fatal(err)
			}
			var c struct {
				ID        string `json:"entry_id"`
				Providers map[string]struct {
					Upstream ProviderSpec `json:"upstream"`
				} `json:"providers"`
			}
			if err = json.Unmarshal(raw, &c); err != nil {
				t.Fatal(err)
			}
			s := c.Providers["fal"].Upstream
			binding := ProviderBinding{Provider: "fal", ID: s.ID, Revision: s.Revision, ContractHash: s.ContractHash}
			cases := []struct {
				name    string
				fields  map[string]any
				invalid bool
				count   int
			}{
				{"omitted", nil, false, 0},
				{"null seed", map[string]any{"seed": nil}, false, 0},
				{"integer seed", map[string]any{"seed": 42}, false, 0},
				{"string seed", map[string]any{"seed": "42"}, true, 0},
				{"fraction seed", map[string]any{"seed": 1.5}, true, 0},
			}
			if model == "turbo-tiling-lora" {
				cases = append(cases,
					struct {
						name    string
						fields  map[string]any
						invalid bool
						count   int
					}{"mask passthrough", map[string]any{"mask_image_url": "https://example.com/mask.png"}, false, 1},
					struct {
						name    string
						fields  map[string]any
						invalid bool
						count   int
					}{"source and mask", map[string]any{"image_url": "https://example.com/source.png", "mask_image_url": "https://example.com/mask.png"}, false, 2},
					struct {
						name    string
						fields  map[string]any
						invalid bool
						count   int
					}{"null mask", map[string]any{"mask_image_url": nil}, false, 0},
					struct {
						name    string
						fields  map[string]any
						invalid bool
						count   int
					}{"number mask", map[string]any{"mask_image_url": 12}, true, 0},
				)
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					payload := map[string]any{"model": c.ID, "prompt": "A red ceramic cup on a table"}
					for k, v := range tc.fields {
						payload[k] = v
					}
					body, _ := json.Marshal(payload)
					normalized, validationErr := ValidateImage(raw, c.ID, body)
					if tc.invalid {
						if validationErr == nil {
							t.Fatal("invalid input accepted")
						}
						return
					}
					if validationErr != nil {
						t.Fatalf("public validation: %+v", validationErr)
					}
					mapped, err := MapBoundProviderInput(raw, binding, "fal-image", s.Endpoint, "async", normalized)
					if err != nil {
						t.Fatalf("provider mapping: %v", err)
					}
					var forwarded map[string]any
					json.Unmarshal(mapped, &forwarded)
					for key, want := range tc.fields {
						actual, exists := forwarded[key]
						a, _ := json.Marshal(actual)
						w, _ := json.Marshal(want)
						if !exists || string(a) != string(w) {
							t.Fatalf("%s changed: %s != %s", key, a, w)
						}
					}
					evidence, err := ResolveImageMetering(raw, binding, mapped, 1, 1024, true)
					if err != nil || evidence.InputCount != int64(tc.count) {
						t.Fatalf("metering: %+v %v", evidence, err)
					}
				})
			}
		})
	}
}
