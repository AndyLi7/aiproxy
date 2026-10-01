package main

import (
	"context"
	"encoding/json"
	rv "github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"net/http"
	"os"
	"testing"
)

func TestOfflineOutputReplayAndNullableFailures(t *testing.T) {
	data, err := os.ReadFile("../../common/registryvalidation/testdata/provider-output-replay.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []sample
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {

		var response map[string]any
		if err := json.Unmarshal(f.Response, &response); err != nil {
			t.Fatal(err)
		}
		if images, ok := response["images"].([]any); ok && len(images) > 0 {
			response["images"] = append(images, images[0])
			bad, _ := json.Marshal(response)
			more := f
			more.Response = bad
			checked := run(more)
			if checked.OutputPassed {
				t.Fatal("excess output count accepted")
			}
		}
		r := run(f)
		if !r.Passed || !r.OutputPassed {
			t.Fatalf("%s: %+v", f.Endpoint, r)
		}
	}
	var c map[string]any
	json.Unmarshal(fixtures[0].Contract, &c)
	p := c["providers"].(map[string]any)["fal"].(map[string]any)["upstream"].(map[string]any)
	p["outputJsonSchema"] = map[string]any{"type": "object", "properties": map[string]any{"image": map[string]any{"anyOf": []any{map[string]any{"type": "object", "required": []string{"url"}, "properties": map[string]any{"url": map[string]any{"type": "string"}}}, map[string]any{"type": "null"}}}}}
	raw, _ := json.Marshal(c)
	binding := rv.ProviderBinding{Provider: "fal", ID: p["id"].(string), Revision: p["revision"].(string), ContractHash: p["contractHash"].(string)}
	frozen, err := rv.FreezeProviderBinding(raw, binding)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{}`, `{"image":null}`, `{"image":{"url":"http://example.com/a.png"}}`, `{"image":{"url":"https://example.com/a.png"},"images":[{"url":"https://example.com/b.png"}]}`} {
		if err = replayOutput(frozen, p["endpoint"].(string), []byte(body), false); err != nil {
			t.Fatal(err)
		}
	}
	if err = replayOutput(frozen, p["endpoint"].(string), []byte(`{"image":{"url":"https://example.com/a.png"}}`), true); err != nil {
		t.Fatal(err)
	}
}
func TestFixtureTransportCannotAccessNetwork(t *testing.T) {
	for _, target := range []string{"https://fal.ai/test", "http://127.0.0.1/test"} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		if _, err := (fixtureTransport{}).RoundTrip(req); err == nil {
			t.Fatal("network fallback")
		}
	}
	req, _ := http.NewRequest(http.MethodPost, "https://offline.invalid/test", nil)
	if _, err := (fixtureTransport{}).RoundTrip(req); err == nil {
		t.Fatal("submission permitted")
	}
}

func TestRealRevisedPromptDelivery(t *testing.T) {
	data, err := os.ReadFile("../../common/registryvalidation/testdata/provider-revised-prompt.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []sample
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 6 {
		t.Fatal("missing fixtures")
	}
	for _, f := range fixtures {
		var c struct {
			Providers map[string]struct{ Upstream rv.ProviderSpec }
		}
		json.Unmarshal(f.Contract, &c)
		p := c.Providers["fal"].Upstream
		frozen, err := rv.FreezeProviderBinding(f.Contract, rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash})
		if err != nil {
			t.Fatal(err)
		}
		var response map[string]any
		json.Unmarshal(f.Response, &response)
		for _, v := range []any{nil, "", "expanded by upstream"} {
			response["revised_prompt"] = v
			payload, _ := json.Marshal(response)
			client := fal.Client{BaseURL: "https://offline.invalid", HTTP: &http.Client{Transport: fixtureTransport{body: payload}}}
			result, err := client.Poll(context.Background(), p.Endpoint, "audit", frozen)
			if err != nil || result.Status != "completed" || len(result.Data) == 0 {
				t.Fatalf("%s: %v %+v", f.Endpoint, err, result)
			}
			expected, _ := v.(string)
			for _, img := range result.Data {
				if img.RevisedPrompt != expected {
					t.Fatalf("%s lost revised prompt", f.Endpoint)
				}
			}
		}
		for _, v := range []any{1, true, map[string]any{}, []any{}} {
			response["revised_prompt"] = v
			payload, _ := json.Marshal(response)
			if err = rv.ValidateFrozenProviderOutput(frozen, payload); err == nil {
				t.Fatal("accepted non-text prompt")
			}
		}
	}
}

func TestRealUsedSeedDelivery(t *testing.T) {
	data, err := os.ReadFile("../../common/registryvalidation/testdata/provider-used-seed.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []sample
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 3 {
		t.Fatal("missing fixtures")
	}
	for _, f := range fixtures {
		var c struct {
			Providers map[string]struct{ Upstream rv.ProviderSpec }
		}
		json.Unmarshal(f.Contract, &c)
		p := c.Providers["fal"].Upstream
		frozen, err := rv.FreezeProviderBinding(f.Contract, rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash})
		if err != nil {
			t.Fatal(err)
		}
		var response map[string]any
		json.Unmarshal(f.Response, &response)
		for _, value := range []string{"0", "42", "18446744073709551615"} {
			response["used_seed"] = json.Number(value)
			body, _ := json.Marshal(response)
			client := fal.Client{BaseURL: "https://offline.invalid", HTTP: &http.Client{Transport: fixtureTransport{body: body}}}
			result, err := client.Poll(context.Background(), p.Endpoint, "audit", frozen)
			if err != nil || result.Status != "completed" {
				t.Fatalf("%s: %v %+v", f.Endpoint, err, result)
			}
			actual := result.Metadata.SeedExact
			if result.Metadata.Seed != nil {
				raw, _ := json.Marshal(*result.Metadata.Seed)
				actual = string(raw)
			}
			if actual != value {
				t.Fatalf("seed lost precision: %s != %s", actual, value)
			}
		}
		for _, value := range []any{1.5, "42", true, map[string]any{}} {
			response["used_seed"] = value
			body, _ := json.Marshal(response)
			if err := rv.ValidateFrozenProviderOutput(frozen, body); err == nil {
				t.Fatal("non-integer seed accepted")
			}
		}
	}
}

func TestDeclaredProviderMetadataDelivery(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/provider.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract map[string]any
	if err = json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	upstream := contract["providers"].(map[string]any)["small"].(map[string]any)["upstream"].(map[string]any)
	upstream["outputMetadata"] = map[string]any{"version": 1, "fields": []string{"caption", "palette", "pixel_scale"}}
	output := upstream["outputJsonSchema"].(map[string]any)
	props := output["properties"].(map[string]any)
	props["caption"] = map[string]any{"type": "string"}
	props["palette"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	props["pixel_scale"] = map[string]any{"type": "integer"}
	raw, _ = json.Marshal(contract)
	frozen, err := rv.FreezeProviderBinding(raw, rv.ProviderBinding{Provider: "small", ID: "small", Revision: "1", ContractHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"images":[{"url":"https://example.com/image.png"}],"caption":"A picture","palette":["#ffffff"],"pixel_scale":0,"unknown":"hidden"}`,
		`{"images":[{"url":"https://example.com/image.png"}],"caption":12}`,
	} {
		client := fal.Client{BaseURL: "https://offline.invalid", HTTP: &http.Client{Transport: fixtureTransport{body: []byte(body)}}}
		result, err := client.Poll(context.Background(), "fal-ai/test", "audit", frozen)
		if err != nil {
			t.Fatal(err)
		}
		var expected map[string]json.RawMessage
		json.Unmarshal([]byte(body), &expected)
		if string(expected["caption"]) == "12" {
			if result.Status != "failed" {
				t.Fatal("invalid metadata accepted")
			}
			continue
		}
		if result.Status != "completed" || len(result.Metadata.ProviderMetadata) != 3 {
			t.Fatalf("metadata not delivered: %+v", result)
		}
		for _, name := range []string{"caption", "palette", "pixel_scale"} {
			if string(result.Metadata.ProviderMetadata[name]) != string(expected[name]) {
				t.Fatalf("metadata changed: %s", name)
			}
		}
	}
}

func TestRealNativeMetadataFieldsAreDelivered(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/provider-native-metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []sample
	if err = json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 14 {
		t.Fatal("missing real metadata scenarios")
	}
	for _, f := range fixtures {
		var contract struct {
			Providers map[string]struct{ Upstream rv.ProviderSpec }
		}
		if err = json.Unmarshal(f.Contract, &contract); err != nil {
			t.Fatal(err)
		}
		p := contract.Providers["fal"].Upstream
		if p.OutputMetadata == nil || len(p.OutputMetadata.Fields) == 0 {
			t.Fatal("missing projection", f.Endpoint)
		}
		frozen, err := rv.FreezeProviderBinding(f.Contract, rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash})
		if err != nil {
			t.Fatal(err)
		}
		var response map[string]json.RawMessage
		json.Unmarshal(f.Response, &response)
		client := fal.Client{BaseURL: "https://offline.invalid", HTTP: &http.Client{Transport: fixtureTransport{body: f.Response}}}
		result, err := client.Poll(context.Background(), p.Endpoint, "audit", frozen)
		if _, hasImage := response["image"]; !hasImage && response["images"] == nil {
			if err != nil || result.Status != "failed" {
				t.Fatalf("text-only result accepted as image: %s", f.Endpoint)
			}
			continue
		}
		if err != nil || result.Status != "completed" {
			t.Fatalf("%s: %v %+v", f.Endpoint, err, result)
		}
		expectedCount := 0
		for _, name := range p.OutputMetadata.Fields {
			expected, present := response[name]
			if present {
				expectedCount++
			}
			got, exists := result.Metadata.ProviderMetadata[name]
			if exists != present || string(got) != string(expected) {
				t.Fatalf("%s: field %s lost or changed: %s vs %s", f.Endpoint, name, got, expected)
			}
		}
		if len(result.Metadata.ProviderMetadata) != expectedCount {
			t.Fatal("undeclared metadata delivered", f.Endpoint)
		}
	}
}

func TestRealUUIDStyleInputAndOutputDomains(t *testing.T) {
	data, err := os.ReadFile("../../common/registryvalidation/testdata/provider-uuid-domain.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []sample
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 4 {
		t.Fatal("missing style fixtures")
	}
	for _, f := range fixtures {
		var c struct {
			Providers map[string]struct{ Upstream rv.ProviderSpec }
		}
		json.Unmarshal(f.Contract, &c)
		p := c.Providers["fal"].Upstream
		binding := rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash}
		frozen, err := rv.FreezeProviderBinding(f.Contract, binding)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			value any
			valid bool
		}{
			{"123e4567-e89b-42d3-a456-426614174000", true},
			{"123E4567-E89B-42D3-A456-426614174000", true}, {nil, true},
			{"123e4567-e89b-12d3-a456-426614174000", false},
			{"123e4567-e89b-72d3-a456-426614174000", false},
			{"123e4567-e89b-42d3-7456-426614174000", false}, {"bad", false}, {42, false},
		} {
			var request map[string]any
			json.Unmarshal(f.Request, &request)
			request["style_id"] = tc.value
			body, _ := json.Marshal(request)
			normalized, validationErr := rv.ValidateImage(f.Contract, request["model"].(string), body)
			if (validationErr == nil) != tc.valid {
				t.Fatalf("%s input %v: %v", f.Endpoint, tc.value, validationErr)
			}
			if tc.valid {
				mapped, err := rv.MapBoundProviderInput(f.Contract, binding, "fal-image", p.Endpoint, "async", normalized)
				if err != nil {
					t.Fatal(err)
				}
				var native map[string]any
				json.Unmarshal(mapped, &native)
				if native["style_id"] != tc.value {
					t.Fatalf("style ID changed: %v", native["style_id"])
				}
			}
			var response map[string]any
			json.Unmarshal(f.Response, &response)
			response["style_id"] = tc.value
			body, _ = json.Marshal(response)
			client := fal.Client{BaseURL: "https://offline.invalid", HTTP: &http.Client{Transport: fixtureTransport{body: body}}}
			result, err := client.Poll(context.Background(), p.Endpoint, "audit", frozen)
			if err != nil {
				t.Fatal(err)
			}
			if (result.Status == "completed") != tc.valid {
				t.Fatalf("%s output %v: %+v", f.Endpoint, tc.value, result)
			}
			if tc.valid {
				expected, _ := json.Marshal(tc.value)
				if string(result.Metadata.ProviderMetadata["style_id"]) != string(expected) {
					t.Fatalf("style ID not delivered: %+v", result.Metadata)
				}
			}
		}
	}
}
