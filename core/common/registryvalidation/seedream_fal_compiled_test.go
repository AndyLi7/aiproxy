//nolint:testpackage
package registryvalidation

import (
	"encoding/json"
	"os"
	"testing"
)

// Use real Registry compiler output, rather than a hand-written gateway contract.
func TestCompiledFalSeedreamContracts(t *testing.T) {
	cases := []struct {
		name    string
		edit    bool
		batch   bool
		inputs  int64
		outputs int
	}{
		{"seedream-5.0-pro-v2-text-to-image", false, false, 0, 2},
		{"seedream-5.0-pro-v2-edit", true, false, 2, 2},
		{"seedream-4.5-v2-text-to-image", false, true, 0, 6},
		{"seedream-4.5-v2-edit", true, true, 2, 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/" + tc.name + ".json")
			if err != nil {
				t.Fatal(err)
			}

			var c struct {
				Providers map[string]struct {
					Upstream ProviderSpec `json:"upstream"`
				} `json:"providers"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				t.Fatal(err)
			}

			s := c.Providers["fal"].Upstream
			b := ProviderBinding{
				Provider:     "fal",
				ID:           s.ID,
				Revision:     s.Revision,
				ContractHash: s.ContractHash,
			}

			input := map[string]any{
				"model":      "public/seedream",
				"prompt":     "A presentation slide",
				"n":          2,
				"image_size": "landscape_16_9",
			}
			if tc.batch {
				input["max_images"] = 3
			} else {
				input["output_format"] = "png"
			}

			if tc.edit {
				input["image_urls"] = []string{
					"https://example.com/a.png",
					"https://example.com/b.png",
				}
			}

			body, _ := json.Marshal(input)

			mapped, err := MapBoundProviderInput(raw, b, "fal-image", s.Endpoint, "async", body)
			if err != nil {
				t.Fatal(err)
			}

			var native map[string]any
			if err := json.Unmarshal(mapped, &native); err != nil {
				t.Fatal(err)
			}

			if native["num_images"] != float64(2) || native["sync_mode"] != false ||
				native["enable_safety_checker"] != true {
				t.Fatalf("bad native request: %s", mapped)
			}

			metering, err := ResolveImageMetering(raw, b, mapped, 2, 1024, true)
			if err != nil || metering.InputCount != tc.inputs ||
				metering.MaximumOutputs != tc.outputs {
				t.Fatalf("%+v %v", metering, err)
			}

			delete(input, "image_size")
			delete(input, "output_format")

			withoutDefaults, _ := json.Marshal(input)
			if _, err := MapBoundProviderInput(
				raw,
				b,
				"fal-image",
				s.Endpoint,
				"async",
				withoutDefaults,
			); err == nil {
				t.Fatal("platform-required size/format omitted but accepted")
			}

			if tc.batch {
				input["image_size"] = "landscape_16_9"
				delete(input, "max_images")

				withoutMultiplier, _ := json.Marshal(input)
				if _, err := MapBoundProviderInput(
					raw,
					b,
					"fal-image",
					s.Endpoint,
					"async",
					withoutMultiplier,
				); err == nil {
					t.Fatal("batch multiplier required for durable maximum")
				}

				input["max_images"] = 3
			}

			frozen, err := FreezeProviderBinding(raw, b)
			if err != nil {
				t.Fatal(err)
			}

			result := `{"images":[{"url":"https://fal.media/out.png","width":null,"height":null}]}`
			if tc.name == "seedream-4.5-v2-text-to-image" {
				result = `{"images":[{"url":"https://fal.media/out.png","width":null,"height":null}],"seed":42}`
			}

			if err := ValidateFrozenProviderOutput(frozen, []byte(result)); err != nil {
				t.Fatalf("%s: %v", result, err)
			}

			if tc.edit {
				input["image_urls"] = []string{}

				bad, _ := json.Marshal(input)
				if _, err := MapBoundProviderInput(
					raw,
					b,
					"fal-image",
					s.Endpoint,
					"async",
					bad,
				); err == nil {
					t.Fatal("empty reference set accepted")
				}
			}

			if tc.batch && tc.edit {
				input["model"] = "bytedance/seedream-4.5-v2/edit"
				input["image_urls"] = []string{
					"https://example.com/1.png",
					"https://example.com/2.png",
					"https://example.com/3.png",
					"https://example.com/4.png",
					"https://example.com/5.png",
					"https://example.com/6.png",
					"https://example.com/7.png",
					"https://example.com/8.png",
					"https://example.com/9.png",
					"https://example.com/10.png",
				}
				input["n"] = 1
				input["max_images"] = 5

				boundary, _ := json.Marshal(input)
				if _, err := ValidateImage(
					raw,
					"bytedance/seedream-4.5-v2/edit",
					boundary,
				); err != nil {
					t.Fatalf("public contract rejected valid 15-image boundary: %v", err)
				}

				input["image_urls"] = []string{
					"https://example.com/a.png",
					"https://example.com/b.png",
				}
				input["n"] = 6
				input["max_images"] = 3

				bad, _ := json.Marshal(input)
				if _, err := ValidateImage(raw, "bytedance/seedream-4.5-v2/edit", bad); err == nil {
					t.Fatal("public contract accepted impossible combined input/output count")
				}

				mappedBad, err := MapBoundProviderInput(
					raw,
					b,
					"fal-image",
					s.Endpoint,
					"async",
					bad,
				)
				if err != nil {
					t.Fatal(err)
				}

				if _, err := ResolveImageMetering(raw, b, mappedBad, 6, 1024, true); err == nil {
					t.Fatal("gateway metering accepted impossible combined input/output count")
				}
			}
		})
	}
}
