package registryvalidation

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestImageMeteringBounds(t *testing.T) {
	for _, tc := range []struct {
		name, meta, body string
		n, want          int
		bad              bool
	}{
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
			got, err := ResolveImageMetering(encoded, fixtureBinding(), []byte(tc.body), tc.n, 1024, true)
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
	_, err := ResolveImageMetering(providerFixture(), fixtureBinding(), []byte(`{"num_images":1,"max_images":3}`), 1, 1024, false)
	if err == nil {
		t.Fatal("batch without metering accepted")
	}
}

func TestImageMeteringRejectsUndeclaredReferences(t *testing.T) {
	for _, key := range []string{"image_url", "image_urls", "reference_image_url", "reference_image_urls", "input_image_url", "input_image_urls"} {
		for _, declared := range []bool{false, true} {
			t.Run(key+fmt.Sprint(declared), func(t *testing.T) {
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
				if _, err := ResolveImageMetering(contract, fixtureBinding(), mapped, 1, 1024, true); err == nil {
					t.Fatal("undeclared reference accepted")
				}
			})
		}
	}
	var raw map[string]any
	if err := json.Unmarshal(providerFixture(), &raw); err != nil {
		t.Fatal(err)
	}
	mustMap(t, mustMap(t, mustMap(t, raw["providers"])["small"])["upstream"])["metering"] = map[string]any{"version": 1}
	contract := mustJSON(t, raw)
	evidence, err := ResolveImageMetering(contract, fixtureBinding(), []byte(`{"num_images":1,"prompt":"text only"}`), 1, 1024, true)
	if err != nil || evidence.InputCount != 0 {
		t.Fatalf("text-only request rejected: %+v %v", evidence, err)
	}
}
