//nolint:testpackage
package registryvalidation

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestImageMeteringBounds(t *testing.T) {
	for _, tc := range []struct {
		name, meta, body string
		n, want          int
		bad              bool
	}{
		{"nullable images omitted", `{"version":5,"outputCountFixed":1,"inputImages":[{"parameter":"image_url","role":"source","shape":"single","required":false,"maximum":1,"nullable":true},{"parameter":"mask_image_url","role":"mask","shape":"single","required":false,"maximum":1,"nullable":true}]}`, `{}`, 1, 1, false},
		{"nullable images null", `{"version":5,"outputCountFixed":1,"inputImages":[{"parameter":"image_url","role":"source","shape":"single","required":false,"maximum":1,"nullable":true},{"parameter":"mask_image_url","role":"mask","shape":"single","required":false,"maximum":1,"nullable":true}]}`, `{"image_url":null,"mask_image_url":null}`, 1, 1, false},
		{"mask requires image", `{"version":5,"outputCountFixed":1,"inputImages":[{"parameter":"image_url","role":"source","shape":"single","required":false,"maximum":1,"nullable":true},{"parameter":"mask_image_url","role":"mask","shape":"single","required":false,"maximum":1,"nullable":true}]}`, `{"mask_image_url":"https://example.com/mask.png"}`, 1, 0, true},
		{"mask with null image", `{"version":5,"outputCountFixed":1,"inputImages":[{"parameter":"image_url","role":"source","shape":"single","required":false,"maximum":1,"nullable":true},{"parameter":"mask_image_url","role":"mask","shape":"single","required":false,"maximum":1,"nullable":true}]}`, `{"image_url":null,"mask_image_url":"https://example.com/mask.png"}`, 1, 0, true},
		{"mask with image", `{"version":5,"outputCountFixed":1,"inputImages":[{"parameter":"image_url","role":"source","shape":"single","required":false,"maximum":1,"nullable":true},{"parameter":"mask_image_url","role":"mask","shape":"single","required":false,"maximum":1,"nullable":true}]}`, `{"image_url":"https://example.com/ref.png","mask_image_url":"https://example.com/mask.png"}`, 1, 1, false},
		{"roles valid", `{"version":5,"outputCountFixed":1,"inputImages":[{"parameter":"image_url","role":"source","shape":"single","required":true,"maximum":1},{"parameter":"mask_url","role":"mask","shape":"single","required":true,"maximum":1}]}`, `{"image_url":"a","mask_url":"b"}`, 1, 1, false},
		{"roles missing mask", `{"version":5,"outputCountFixed":1,"inputImages":[{"parameter":"image_url","role":"source","shape":"single","required":true,"maximum":1},{"parameter":"mask_url","role":"mask","shape":"single","required":true,"maximum":1}]}`, `{"image_url":"a"}`, 1, 0, true},
		{"roles null mask", `{"version":5,"outputCountFixed":1,"inputImages":[{"parameter":"image_url","role":"source","shape":"single","required":true,"maximum":1},{"parameter":"mask_url","role":"mask","shape":"single","required":true,"maximum":1}]}`, `{"image_url":"a","mask_url":null}`, 1, 0, true},
		{"roles array mask", `{"version":5,"outputCountFixed":1,"inputImages":[{"parameter":"image_url","role":"source","shape":"single","required":true,"maximum":1},{"parameter":"mask_url","role":"mask","shape":"single","required":true,"maximum":1}]}`, `{"image_url":"a","mask_url":["b"]}`, 1, 0, true},
		{"roles excess samples", `{"version":5,"outputCountFixed":1,"inputImages":[{"parameter":"image_url","role":"source","shape":"single","required":true,"maximum":1},{"parameter":"mask_url","role":"mask","shape":"single","required":true,"maximum":1}]}`, `{"image_url":"a","mask_url":"b","num_samples":2}`, 1, 0, true},
		{"roles invalid role", `{"version":5,"outputCountFixed":1,"inputImages":[{"parameter":"image_url","role":"source","shape":"single","required":true,"maximum":1},{"parameter":"mask_url","role":"person","shape":"single","required":true,"maximum":1}]}`, `{"image_url":"a","mask_url":"b"}`, 1, 0, true},
		{"roles old version", `{"version":3,"outputCountFixed":1,"inputImages":[{"parameter":"image_url","role":"source","shape":"single","required":true,"maximum":1},{"parameter":"mask_url","role":"mask","shape":"single","required":true,"maximum":1}]}`, `{"image_url":"a","mask_url":"b"}`, 1, 0, true},
		{"roles optional omitted", `{"version":5,"outputCountFixed":1,"inputImages":[{"parameter":"image_url","role":"source","shape":"single","required":true,"maximum":1},{"parameter":"mask_url","role":"mask","shape":"single","required":false,"maximum":1}]}`, `{"image_url":"a"}`, 1, 1, false},
		{"pixel v4", `{"version":4,"outputCountParameter":"num_images","maxOutputPixels":4194304}`, `{"num_images":2}`, 2, 2, false},
		{"v4 missing cap", `{"version":4,"outputCountParameter":"num_images"}`, `{"num_images":2}`, 2, 0, true},
		{"v4 invalid cap", `{"version":4,"outputCountParameter":"num_images","maxOutputPixels":0}`, `{"num_images":2}`, 2, 0, true},
		{"old version with cap", `{"version":3,"outputCountParameter":"num_images","maxOutputPixels":4194304}`, `{"num_images":2}`, 2, 0, true},
		{"optional v3 omitted", `{"version":3,"outputCountParameter":"num_images","inputImagesParameter":"image_url","inputImagesShape":"single","inputImagesOptional":true,"maxInputImages":1}`, `{"num_images":1}`, 1, 1, false},
		{"optional v3 null rejected", `{"version":3,"outputCountParameter":"num_images","inputImagesParameter":"image_url","inputImagesShape":"single","inputImagesOptional":true,"maxInputImages":1}`, `{"num_images":1,"image_url":null}`, 1, 0, true},
		{"optional v2 rejected", `{"version":2,"outputCountParameter":"num_images","inputImagesParameter":"image_url","inputImagesShape":"single","inputImagesOptional":true,"maxInputImages":1}`, `{"num_images":1}`, 1, 0, true},
		{"optional v3 empty array rejected", `{"version":3,"outputCountParameter":"num_images","inputImagesParameter":"image_urls","inputImagesShape":"array","inputImagesOptional":true,"maxInputImages":2}`, `{"num_images":1,"image_urls":[]}`, 1, 0, true},
		{"optional v3 combined cap preserved", `{"version":3,"outputCountParameter":"num_images","inputImagesParameter":"image_urls","inputImagesShape":"array","inputImagesOptional":true,"maxInputImages":2,"maxCombinedImages":1}`, `{"num_images":2}`, 2, 0, true},
		{"fixed v3", `{"version":3,"outputCountFixed":1}`, `{}`, 1, 1, false},
		{"fixed v3 rejects multiple", `{"version":3,"outputCountFixed":1}`, `{}`, 2, 0, true},
		{"fixed v3 exclusive", `{"version":3,"outputCountFixed":1,"outputCountParameter":"num_images"}`, `{"num_images":1}`, 1, 0, true},
		{"fixed v2 rejected", `{"version":2,"outputCountFixed":1}`, `{}`, 1, 0, true},
		{"fixed v3 rejects native count", `{"version":3,"outputCountFixed":1}`, `{"num_images":2}`, 1, 0, true},
		{"fixed v3 rejects multiplier", `{"version":3,"outputCountFixed":1,"outputCountMultiplierParameter":"max_images"}`, `{"max_images":2}`, 1, 0, true},
		{"single v2", `{"version":2,"outputCountParameter":"num_images","inputImagesParameter":"image_url","inputImagesShape":"single","maxInputImages":1}`, `{"num_images":2,"image_url":"https://example.com/ref.png"}`, 2, 2, false},
		{"wrong shape v2", `{"version":2,"outputCountParameter":"num_images","inputImagesParameter":"image_url","inputImagesShape":"single","maxInputImages":1}`, `{"num_images":2,"image_url":["ref"]}`, 2, 0, true},
		{"empty single v2", `{"version":2,"outputCountParameter":"num_images","inputImagesParameter":"image_url","inputImagesShape":"single","maxInputImages":1}`, `{"num_images":2,"image_url":" "}`, 2, 0, true},
		{"v1 stays array only", `{"version":1,"inputImagesParameter":"image_url","maxInputImages":1}`, `{"num_images":2,"image_url":"ref"}`, 2, 0, true},
		{"batch", `{"version":1,"outputCountMultiplierParameter":"max_images"}`, `{"num_images":2,"max_images":3}`, 2, 6, false},
		{"overflow", `{"version":1,"outputCountMultiplierParameter":"max_images"}`, `{"num_images":2,"max_images":9007199254740991}`, 2, 0, true},
		{"missing", `{"version":1,"outputCountMultiplierParameter":"max_images"}`, `{"num_images":2}`, 2, 0, true},
		{"count mismatch", `{"version":1}`, `{"num_images":3}`, 2, 0, true},
		{"references", `{"version":1,"inputImagesParameter":"image_urls","maxInputImages":2}`, `{"num_images":2,"image_urls":["a","b"]}`, 2, 2, false},
		{"input cap", `{"version":1,"inputImagesParameter":"image_urls","maxInputImages":1}`, `{"num_images":2,"image_urls":["a","b"]}`, 2, 0, true},
		{"combined cap", `{"version":1,"inputImagesParameter":"image_urls","maxCombinedImages":3}`, `{"num_images":2,"image_urls":["a","b"]}`, 2, 0, true},
		{"null", `{"version":1,"inputImagesParameter":null}`, `{"num_images":2}`, 2, 0, true},
		{"empty", `{"version":1,"inputImagesParameter":""}`, `{"num_images":2}`, 2, 0, true},
		{"hyphen", `{"version":1,"inputImagesParameter":"x-y"}`, `{"num_images":2}`, 2, 0, true},
		{"unknown", `{"version":2}`, `{"num_images":2}`, 2, 0, true},
		{"nested", `{"version":1,"inputImagesParameter":"x.y"}`, `{"num_images":2}`, 2, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal(providerFixture(), &raw); err != nil {
				t.Fatal(err)
			}

			upstream := mustMap(t, mustMap(t, mustMap(t, raw["providers"])["small"])["upstream"])

			var meta any
			if err := json.Unmarshal([]byte(tc.meta), &meta); err != nil {
				t.Fatal(err)
			}

			upstream["metering"] = meta
			encoded := mustJSON(t, raw)

			got, err := ResolveImageMetering(
				encoded,
				fixtureBinding(),
				[]byte(tc.body),
				tc.n,
				1024,
				true,
			)
			if (err != nil) != tc.bad {
				t.Fatalf("err=%v", err)
			}

			if err == nil && got.MaximumOutputs != tc.want {
				t.Fatalf("got=%+v", got)
			}
		})
	}
}

func TestBatchParameterRequiresExplicitMetering(t *testing.T) {
	_, err := ResolveImageMetering(
		providerFixture(),
		fixtureBinding(),
		[]byte(`{"num_images":1,"max_images":3}`),
		1,
		1024,
		false,
	)
	if err == nil {
		t.Fatal("batch without metering accepted")
	}
}

func TestImageMeteringRejectsUndeclaredReferences(t *testing.T) {
	for _, key := range []string{"image_url", "image_urls", "reference_image_url", "reference_image_urls", "input_image_url", "input_image_urls"} {
		for _, declared := range []bool{false, true} {
			t.Run(key+strconv.FormatBool(declared), func(t *testing.T) {
				var raw map[string]any
				if err := json.Unmarshal(providerFixture(), &raw); err != nil {
					t.Fatal(err)
				}

				metadata := map[string]any{"version": 1}

				body := map[string]any{"num_images": 1, key: "https://cdn.example/reference"}
				if strings.HasSuffix(key, "urls") {
					body[key] = []string{"https://cdn.example/reference"}
				}

				if declared {
					metadata["inputImagesParameter"] = "declared_refs"
					body["declared_refs"] = []string{"https://cdn.example/declared"}
				}

				mustMap(t, mustMap(t, mustMap(t, raw["providers"])["small"])["upstream"])["metering"] = metadata
				contract := mustJSON(t, raw)

				mapped := mustJSON(t, body)
				if _, err := ResolveImageMetering(
					contract,
					fixtureBinding(),
					mapped,
					1,
					1024,
					true,
				); err == nil {
					t.Fatal("undeclared reference accepted")
				}
			})
		}
	}

	var raw map[string]any
	if err := json.Unmarshal(providerFixture(), &raw); err != nil {
		t.Fatal(err)
	}

	mustMap(t, mustMap(t, mustMap(t, raw["providers"])["small"])["upstream"])["metering"] = map[string]any{
		"version": 1,
	}
	contract := mustJSON(t, raw)

	evidence, err := ResolveImageMetering(
		contract,
		fixtureBinding(),
		[]byte(`{"num_images":1,"prompt":"text only"}`),
		1,
		1024,
		true,
	)
	if err != nil || evidence.InputCount != 0 {
		t.Fatalf("text-only request rejected: %+v %v", evidence, err)
	}
}
