//nolint:testpackage
package registryvalidation

import (
	"encoding/json"
	"os"
	"testing"
)

// This file consumes the exact gateway contract emitted by the TS Registry compiler.
func TestCompiledMediaProviderContract(t *testing.T) {
	raw, err := os.ReadFile("testdata/provider-media.json")
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

	mapped, err := MapBoundProviderInput(
		raw,
		b,
		"fal-image",
		s.Endpoint,
		"async",
		[]byte(
			`{"model":"test/media/edit","prompt":"test","n":2,"max_images":3,"image_urls":["https://example.com/a.png","https://example.com/b.png"]}`,
		),
	)
	if err != nil {
		t.Fatal(err)
	}

	evidence, err := ResolveImageMetering(raw, b, mapped, 2, 1024, true)
	if err != nil || evidence.InputCount != 2 || evidence.MaximumOutputs != 6 ||
		!evidence.Explicit {
		t.Fatalf("%+v %v", evidence, err)
	}

	for _, bad := range []string{
		`{"num_images":6,"max_images":3,"image_urls":["https://example.com/a.png"]}`,
		`{"num_images":2,"max_images":0,"image_urls":["https://example.com/a.png"]}`,
		`{"num_images":2,"max_images":3,"image_urls":[]}`,
	} {
		if _, err := ResolveImageMetering(raw, b, []byte(bad), 2, 1024, true); err == nil {
			t.Fatal("invalid or mismatched batch accepted")
		}
	}

	combined := []byte(`{"num_images":6,"max_images":3,"image_urls":["https://example.com/a.png"]}`)
	if _, err := ResolveImageMetering(raw, b, combined, 6, 1024, true); err == nil {
		t.Fatal("combined cap ignored")
	}
}
