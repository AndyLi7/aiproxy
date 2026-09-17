//nolint:testpackage
package registryvalidation

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func mustMap(t *testing.T, value any) map[string]any {
	t.Helper()

	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T", value)
	}

	return result
}

func providerFixture() []byte {
	raw, err := os.ReadFile("testdata/provider.json")
	if err != nil {
		panic(err)
	}

	return raw
}

func fixtureBinding() ProviderBinding {
	return ProviderBinding{
		Provider:     "small",
		ID:           "small",
		Revision:     "1",
		ContractHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
}

func TestVersionedPublicNormalization(t *testing.T) {
	body, err := ValidateImage(
		providerFixture(),
		"image",
		[]byte(`{"model":"image","prompt":"hi"}`),
	)
	if err != nil {
		t.Fatal(err)
	}

	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}

	if value["n"] != float64(1) || value["sync_mode"] != nil {
		t.Fatalf("unexpected %s", body)
	}

	if _, err = ValidateImage(
		providerFixture(),
		"image",
		[]byte(`{"model":"image","prompt":"hi","sync_mode":false}`),
	); err == nil {
		t.Fatal("native constant accepted")
	}
}

func TestProviderBindingAndLosslessMapping(t *testing.T) {
	raw := providerFixture()
	b := fixtureBinding()
	body := []byte(`{"model":"image","prompt":"hi","n":1}`)

	mapped, err := MapBoundProviderInput(raw, b, "fal-image", "fal-ai/test", "async", body)
	if err != nil {
		t.Fatal(err)
	}

	if string(mapped) != `{"num_images":1,"prompt":"hi","sync_mode":false}` {
		t.Fatalf("mapping %s", mapped)
	}

	for _, input := range []string{`{"model":"image","prompt":"hi","n":2}`, `{"model":"image","prompt":"hi","n":1,"extra":true}`} {
		if _, err := MapBoundProviderInput(
			raw,
			b,
			"fal-image",
			"fal-ai/test",
			"async",
			[]byte(input),
		); err == nil {
			t.Fatal("incompatible accepted")
		}
	}

	for _, change := range []func(*ProviderBinding){func(b *ProviderBinding) { b.Revision = "2" }, func(b *ProviderBinding) { b.ID = "other" }, func(b *ProviderBinding) { b.Provider = "missing" }, func(b *ProviderBinding) { b.ContractHash = "bad" }} {
		bad := b
		change(&bad)

		if _, err := MapBoundProviderInput(
			raw,
			bad,
			"fal-image",
			"fal-ai/test",
			"async",
			body,
		); err == nil {
			t.Fatal("stale binding accepted")
		}
	}

	for _, a := range [][3]string{{"other", "fal-ai/test", "async"}, {"fal-image", "other", "async"}, {"fal-image", "fal-ai/test", "sync"}} {
		if _, err := MapBoundProviderInput(raw, b, a[0], a[1], a[2], body); err == nil {
			t.Fatal("bad route accepted")
		}
	}
}

func TestFrozenProviderOutput(t *testing.T) {
	frozen, err := FreezeProviderBinding(providerFixture(), fixtureBinding())
	if err != nil {
		t.Fatal(err)
	}

	if err := ValidateFrozenProviderOutput(frozen, []byte(`{"images":[{}]}`)); err != nil {
		t.Fatal(err)
	}

	if err := ValidateFrozenProviderOutput(frozen, []byte(`{"images":[]}`)); err == nil {
		t.Fatal("invalid output accepted")
	}

	if err := ValidateFrozenProviderOutput(
		providerFixture(),
		[]byte(`{"images":[{}]}`),
	); err == nil {
		t.Fatal("missing binding accepted")
	}
}

func TestInvalidSchemaAndFrozenSizeFailBeforeSubmission(t *testing.T) {
	var c map[string]any
	if err := json.Unmarshal(providerFixture(), &c); err != nil {
		t.Fatal(err)
	}

	providers := mustMap(t, c["providers"])
	upstream := mustMap(t, mustMap(t, providers["small"])["upstream"])
	upstream["outputJsonSchema"] = map[string]any{"$ref": "https://example.com/remote.json"}

	bad := mustJSON(t, c)
	if _, err := MapBoundProviderInput(
		bad,
		fixtureBinding(),
		"fal-image",
		"fal-ai/test",
		"async",
		[]byte(`{"model":"image","prompt":"hi","n":1}`),
	); err == nil {
		t.Fatal("external output schema accepted")
	}

	raw := providerFixture()

	var padded map[string]any
	if err := json.Unmarshal(raw, &padded); err != nil {
		t.Fatal(err)
	}

	padded["padding"] = ""
	base := mustJSON(t, padded)
	padded["padding"] = strings.Repeat("x", 256*1024-len(base))

	raw = mustJSON(t, padded)
	if len(raw) != 256*1024 {
		t.Fatal("invalid size fixture")
	}

	if _, err := FreezeProviderBinding(raw, fixtureBinding()); err == nil {
		t.Fatal("oversized frozen contract accepted")
	}
}

func TestSyncProviderRequiresExplicitNativeExecutionFlags(t *testing.T) {
	var c map[string]any
	if err := json.Unmarshal(providerFixture(), &c); err != nil {
		t.Fatal(err)
	}

	p := mustMap(t, mustMap(t, c["providers"])["small"])
	p["adapter"] = "volcengine-ark-image"
	s := mustMap(t, p["upstream"])
	s["endpoint"] = "seedream-test"
	s["execution"] = map[string]any{"mode": "sync"}
	p["fixedParameters"] = map[string]any{"stream": false, "response_format": "url"}
	s["inputJsonSchema"] = map[string]any{"type": "object", "additionalProperties": true}
	raw := mustJSON(t, c)

	_, err := MapBoundProviderInput(
		raw,
		fixtureBinding(),
		"volcengine-ark-image",
		"seedream-test",
		"sync",
		[]byte(`{"prompt":"x","n":1}`),
	)
	if err != nil {
		t.Fatal(err)
	}

	p["fixedParameters"] = map[string]any{"stream": true, "response_format": "url"}

	raw = mustJSON(t, c)
	if _, err = MapBoundProviderInput(
		raw,
		fixtureBinding(),
		"volcengine-ark-image",
		"seedream-test",
		"sync",
		[]byte(`{"prompt":"x","n":1}`),
	); err == nil {
		t.Fatal("stream enabled")
	}
}

func TestFalFullProtocolMappingAndFrozenPathIsolation(t *testing.T) {
	var c map[string]any
	if err := json.Unmarshal(providerFixture(), &c); err != nil {
		t.Fatal(err)
	}

	s := mustMap(t, mustMap(t, mustMap(t, c["providers"])["small"])["upstream"])
	endpoint := "bytedance/seedream/v5/pro/edit"
	s["endpoint"] = endpoint
	e := mustMap(t, s["execution"])
	e["supportsCancellation"] = true
	root := endpoint + "/requests/{request_id}"
	e["resultEndpoint"] = root
	e["statusEndpoint"] = root + "/status"

	raw := mustJSON(t, c)
	if _, err := MapBoundProviderInput(
		raw,
		fixtureBinding(),
		"fal-image",
		endpoint,
		"async",
		[]byte(`{"model":"image","prompt":"hi","n":1}`),
	); err != nil {
		t.Fatal(err)
	}

	frozen, err := FreezeProviderBinding(raw, fixtureBinding())
	if err != nil {
		t.Fatal(err)
	}

	status, result, err := FrozenFalQueuePaths(frozen, endpoint, "abc")
	if err != nil || result != endpoint+"/requests/abc" || status != result+"/status" {
		t.Fatalf("%s %s %v", status, result, err)
	}

	for _, args := range [][2]string{{"other/model", "abc"}, {endpoint, "../secret"}, {endpoint, "a?key=secret"}} {
		if _, _, err := FrozenFalQueuePaths(frozen, args[0], args[1]); err == nil {
			t.Fatal("unsafe or mismatched task accepted")
		}
	}

	for _, bad := range []string{"https://evil.example/requests/{request_id}", "other/model/requests/{request_id}", endpoint + "/../requests/{request_id}", root + "?token=secret"} {
		e["resultEndpoint"] = bad
		e["statusEndpoint"] = bad + "/status"

		raw = mustJSON(t, c)
		if _, err := MapBoundProviderInput(
			raw,
			fixtureBinding(),
			"fal-image",
			endpoint,
			"async",
			[]byte(`{"model":"image","prompt":"hi","n":1}`),
		); err == nil {
			t.Fatal("unsafe submission contract accepted")
		}

		frozen, err := FreezeProviderBinding(raw, fixtureBinding())
		if err != nil {
			t.Fatal(err)
		}

		if _, _, err := FrozenFalQueuePaths(frozen, endpoint, "abc"); err == nil {
			t.Fatal("unsafe polling contract accepted")
		}
	}
}
