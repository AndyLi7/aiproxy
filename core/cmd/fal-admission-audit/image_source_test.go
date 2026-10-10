package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestOfficialBriaImageSourceGuardsRootAndNestedURLs(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/bria-embed-product.json")
	if err != nil {
		t.Fatal(err)
	}
	var s sample
	if err = json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"https://example.com/product.png", "http://127.0.0.1/private", "https://localhost/private", "https://user:pass@example.com/private", "data:image/png;base64,AAAA"} {
		for _, nested := range []bool{false, true} {
			var request map[string]any
			if err = json.Unmarshal(s.Request, &request); err != nil {
				t.Fatal(err)
			}
			request["image_source"] = "https://example.com/background.png"
			product := map[string]any{"image_source": "https://example.com/product.png", "coordinates": map[string]any{"x": 300, "y": 317, "width": 100, "height": 300}}
			if nested {
				product["image_source"] = url
			} else {
				request["image_source"] = url
			}
			request["products"] = []any{product}
			changed := s
			changed.Request, _ = json.Marshal(request)
			result := run(changed)
			if result.Passed != (url == "https://example.com/product.png") {
				t.Fatalf("url=%q nested=%v result=%+v", url, nested, result)
			}
		}
	}
}
