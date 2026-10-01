package main

import (
	"encoding/base64"
	"encoding/json"
	rv "github.com/labring/aiproxy/core/common/registryvalidation"
	"os"
	"strings"
	"testing"
)

func TestRealBriaInlineImagesSurviveMappingAndMetering(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/provider-inline-images.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []sample
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 3 {
		t.Fatal("expected three official Bria source contracts")
	}
	image := "UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA"
	for _, f := range cases {
		var contract struct {
			Providers map[string]struct{ Upstream rv.ProviderSpec }
		}
		json.Unmarshal(f.Contract, &contract)
		p := contract.Providers["fal"].Upstream
		binding := rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash}
		for _, tc := range []struct {
			value string
			valid bool
		}{{image, true}, {image[:20] + "\r\n" + image[20:], true}, {"data:image/webp;base64," + image, false}, {"https://example.com/a.webp", false}, {"http://127.0.0.1/a", false}, {base64.StdEncoding.EncodeToString([]byte("not an image")), false}, {"", false}, {strings.Repeat("A", 16000004), false}} {
			var request map[string]any
			json.Unmarshal(f.Request, &request)
			request["guidance"] = []any{map[string]any{"image_url": tc.value, "method": "controlnet_canny", "scale": 0.5}}
			body, _ := json.Marshal(request)
			normalized, validationErr := rv.ValidateImage(f.Contract, request["model"].(string), body)
			if validationErr != nil {
				t.Fatal(validationErr)
			}
			mapped, err := rv.MapBoundProviderInput(f.Contract, binding, "fal-image", p.Endpoint, "async", normalized)
			if err != nil {
				t.Fatal(err)
			}
			var native map[string]any
			json.Unmarshal(mapped, &native)
			if native["guidance"].([]any)[0].(map[string]any)["image_url"] != tc.value {
				t.Fatal("image bytes changed")
			}
			evidence, err := rv.ResolveImageMetering(f.Contract, binding, mapped, 1, 4, true)
			if (err == nil) != tc.valid {
				t.Fatalf("%s validity %t: %v", f.Endpoint, tc.valid, err)
			}
			if tc.valid && evidence.InputCount != 1 {
				t.Fatalf("bad input count: %+v", evidence)
			}
		}
	}
}
