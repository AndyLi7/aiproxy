package registryvalidation

import (
	"encoding/json"
	"os"
	"testing"
)

// Fixture is produced by compileImportedDefinition, including its versioned binding.
func TestCompiledNativeProviderContract(t *testing.T) {
	raw, err := os.ReadFile("testdata/provider-native-images.json")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Providers map[string]struct {
			Upstream ProviderSpec `json:"upstream"`
		} `json:"providers"`
	}
	if err = json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	s := c.Providers["fal"].Upstream
	b := ProviderBinding{Provider: "fal", ID: s.ID, Revision: s.Revision, ContractHash: s.ContractHash}
	request := []byte(`{"model":"test/native-images/text-to-image","prompt":"test","n":2,"max_images":3,"elements":[{"frontal_image_url":"https://example.com/a.png","reference_image_urls":["https://example.com/b.png"]}]}`)
	mapped, err := MapBoundProviderInput(raw, b, "fal-image", s.Endpoint, "async", request)
	if err != nil {
		t.Fatal(err)
	}
	var original, forwarded map[string]any
	json.Unmarshal(request, &original)
	json.Unmarshal(mapped, &forwarded)
	before, _ := json.Marshal(original["elements"])
	after, _ := json.Marshal(forwarded["elements"])
	if string(before) != string(after) {
		t.Fatalf("nested parameters changed: %s", after)
	}
	evidence, err := ResolveImageMetering(raw, b, mapped, 2, 1024, true)
	if err != nil || evidence.InputCount != 2 || evidence.MaximumOutputs != 6 {
		t.Fatalf("%+v %v", evidence, err)
	}
	for _, bad := range []string{
		`{"model":"test/native-images/text-to-image","prompt":"test","n":2,"elements":[{}]}`,
		`{"model":"test/native-images/text-to-image","prompt":"test","n":2,"elements":[{"frontal_image_url":17}]}`,
	} {
		if _, err = MapBoundProviderInput(raw, b, "fal-image", s.Endpoint, "async", []byte(bad)); err == nil {
			t.Fatal("invalid nested payload accepted")
		}
	}
}

// Fixture is produced by compileImportedDefinition, including its versioned binding.
func TestCompiledExtendedNativeProviderContract(t *testing.T) {
	raw, err := os.ReadFile("testdata/provider-native-images-v7.json")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Providers map[string]struct {
			Upstream ProviderSpec `json:"upstream"`
		} `json:"providers"`
	}
	if err = json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	s := c.Providers["fal"].Upstream
	b := ProviderBinding{Provider: "fal", ID: s.ID, Revision: s.Revision, ContractHash: s.ContractHash}
	request := []byte(`{"model":"test/native-images/text-to-image","prompt":"test","n":2,"max_images":3,"fill_image":{"fill_image_url":"https://example.com/fill.png"},"elements":[{"frontal_image_url":"https://example.com/a.png","reference_image_urls":["https://example.com/b.png"]}]}`)
	mapped, err := MapBoundProviderInput(raw, b, "fal-image", s.Endpoint, "async", request)
	if err != nil {
		t.Fatal(err)
	}
	var original, forwarded map[string]any
	json.Unmarshal(request, &original)
	json.Unmarshal(mapped, &forwarded)
	before, _ := json.Marshal(original["elements"])
	after, _ := json.Marshal(forwarded["elements"])
	if string(before) != string(after) {
		t.Fatalf("nested parameters changed: %s", after)
	}
	evidence, err := ResolveImageMetering(raw, b, mapped, 2, 1024, true)
	if err != nil || evidence.InputCount != 3 || evidence.MaximumOutputs != 6 {
		t.Fatalf("%+v %v", evidence, err)
	}
	for _, bad := range []string{
		`{"model":"test/native-images/text-to-image","prompt":"test","n":2,"elements":[{}]}`,
		`{"model":"test/native-images/text-to-image","prompt":"test","n":2,"elements":[{"frontal_image_url":17}]}`,
	} {
		if _, err = MapBoundProviderInput(raw, b, "fal-image", s.Endpoint, "async", []byte(bad)); err == nil {
			t.Fatal("invalid nested payload accepted")
		}
	}
}
