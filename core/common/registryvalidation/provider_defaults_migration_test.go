package registryvalidation

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestProviderDefaultsAreAnnotationsDuringMapping(t *testing.T) {
	var contract map[string]any
	if err := json.Unmarshal(providerFixture(), &contract); err != nil {
		t.Fatal(err)
	}
	provider := mustMap(t, mustMap(t, contract["providers"])["small"])
	input := mustMap(t, mustMap(t, provider["upstream"])["inputJsonSchema"])
	properties := mustMap(t, input["properties"])
	properties["optional_hint"] = map[string]any{"type": "string", "default": "not sent"}
	body := []byte(`{"model":"image","prompt":"hi","n":1}`)
	withDefault, err := MapBoundProviderInput(mustJSON(t, contract), fixtureBinding(), "fal-image", "fal-ai/test", "async", body)
	if err != nil {
		t.Fatal(err)
	}
	delete(mustMap(t, properties["optional_hint"]), "default")
	withoutDefault, err := MapBoundProviderInput(mustJSON(t, contract), fixtureBinding(), "fal-image", "fal-ai/test", "async", body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(withDefault, withoutDefault) || bytes.Contains(withDefault, []byte("optional_hint")) {
		t.Fatalf("provider default changed payload: %s / %s", withDefault, withoutDefault)
	}
}
