package registryvalidation

import (
	"encoding/json"
	"os"
	"testing"
)

func TestCompiledSourceSchemaKeywords(t *testing.T) {
	data, err := os.ReadFile("testdata/provider-schema-keywords.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Endpoint string          `json:"endpoint"`
		Contract json.RawMessage `json:"contract"`
		Request  json.RawMessage `json:"request"`
	}
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 3 {
		t.Fatal("missing real-source fixtures")
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Endpoint, func(t *testing.T) {
			var c struct {
				Providers map[string]struct {
					Upstream ProviderSpec `json:"upstream"`
				} `json:"providers"`
			}
			if err := json.Unmarshal(fixture.Contract, &c); err != nil {
				t.Fatal(err)
			}
			s := c.Providers["fal"].Upstream
			b := ProviderBinding{Provider: "fal", ID: s.ID, Revision: s.Revision, ContractHash: s.ContractHash}
			mapped, err := MapBoundProviderInput(fixture.Contract, b, "fal-image", s.Endpoint, "async", fixture.Request)
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := ResolveImageMetering(fixture.Contract, b, mapped, 1, 1024, true)
			if err != nil || evidence.MaximumOutputs < 1 {
				t.Fatalf("%+v %v", evidence, err)
			}
			var req, forwarded map[string]any
			json.Unmarshal(fixture.Request, &req)
			json.Unmarshal(mapped, &forwarded)
			if _, exists := forwarded["n"]; exists {
				t.Fatal("platform count leaked upstream")
			}
			var meter ImageMetering
			json.Unmarshal(s.Metering, &meter)
			if meter.OutputCountFixed != nil {
				req["n"] = 2
				bad, _ := json.Marshal(req)
				if _, err := MapBoundProviderInput(fixture.Contract, b, "fal-image", s.Endpoint, "async", bad); err == nil {
					t.Fatal("fixed count accepted n=2")
				}
			}
			for _, key := range []string{"image_url", "image_urls", "aspect_ratio"} {
				if v, ok := req[key]; ok {
					before, _ := json.Marshal(v)
					after, _ := json.Marshal(forwarded[key])
					if string(before) != string(after) {
						t.Fatalf("changed %s", key)
					}
				}
			}
		})
	}
}
