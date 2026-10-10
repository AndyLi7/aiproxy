package registryvalidation

import (
	"encoding/json"
	"os"
	"testing"
)

func TestCompiledEmptyImageDefaultsV8(t *testing.T) {
	data, err := os.ReadFile("testdata/provider-empty-images-v8.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Endpoint, Scenario string
		Contract, Request  json.RawMessage
	}
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 10 {
		t.Fatal("missing real source cases")
	}
	for _, f := range fixtures {
		t.Run(f.Endpoint+"/"+f.Scenario, func(t *testing.T) {
			var c struct {
				ID        string `json:"entry_id"`
				Providers map[string]struct {
					Upstream ProviderSpec `json:"upstream"`
				}
			}
			if err := json.Unmarshal(f.Contract, &c); err != nil {
				t.Fatal(err)
			}
			p := c.Providers["fal"].Upstream
			b := ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash}
			normalized, ve := ValidateImage(f.Contract, c.ID, f.Request)
			if ve != nil {
				t.Fatal(ve)
			}
			mapped, err := MapBoundProviderInput(f.Contract, b, "fal-image", p.Endpoint, "async", normalized)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = ResolveImageMetering(f.Contract, b, mapped, 1, 1024, true); err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			json.Unmarshal(mapped, &payload)
			var m ImageMetering
			json.Unmarshal(p.Metering, &m)
			props := p.Input["properties"].(map[string]any)
			for _, path := range m.NativeImagePaths {
				if len(path) != 1 {
					continue
				}
				field := props[path[0]].(map[string]any)
				if !declaresEmptyImage(field) {
					continue
				}
				payload[path[0]] = ""
				emptyCount, err := resolveNativeImages(m.NativeImagePaths, p.Input, payload, 8)
				if err != nil {
					t.Fatal(err)
				}
				payload[path[0]] = "https://example.com/reference.png"
				fullCount, err := resolveNativeImages(m.NativeImagePaths, p.Input, payload, 8)
				if err != nil || fullCount != emptyCount+1 {
					t.Fatalf("count: %d %d %v", emptyCount, fullCount, err)
				}
				for _, bad := range []string{"", " ", "garbage", "http://127.0.0.1/a.png", "http://localhost/a.png", "http://10.0.0.1/a.png", "https://user:pass@example.com/a.png", "data:image/png;base64,AA=="} {
					payload[path[0]] = bad
					if _, err := resolveNativeImages(m.NativeImagePaths, p.Input, payload, 7); err == nil {
						t.Fatalf("v7 accepted %q", bad)
					}
					if bad != "" {
						if _, err := resolveNativeImages(m.NativeImagePaths, p.Input, payload, 8); err == nil {
							t.Fatalf("v8 accepted %q", bad)
						}
					}
				}
				payload[path[0]] = ""
				oldRequired := p.Input["required"]
				p.Input["required"] = append(arrayValues(oldRequired), path[0])
				if _, err := resolveNativeImages(m.NativeImagePaths, p.Input, payload, 8); err == nil {
					t.Fatal("required empty accepted")
				}
				p.Input["required"] = oldRequired
			}
		})
	}
}
