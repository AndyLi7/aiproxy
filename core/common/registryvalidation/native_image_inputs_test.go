package registryvalidation

import (
	"encoding/json"
	"testing"
)

func TestNativeNestedImages(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"elements":{"type":"array","items":{"type":"object","properties":{"frontal_image_url":{"type":"string"},"reference_image_urls":{"anyOf":[{"type":"array","items":{"type":"string"}},{"type":"null"}]}}}}}}`), &schema); err != nil {
		t.Fatal(err)
	}
	paths := [][]string{{"elements", "*", "frontal_image_url"}, {"elements", "*", "reference_image_urls", "*"}}
	for _, tc := range []struct {
		body  string
		count int64
		bad   bool
	}{
		{`{"elements":[{"frontal_image_url":"https://example.com/a","reference_image_urls":["https://example.com/b","https://example.com/c"]}]}`, 3, false},
		{`{"elements":[{"frontal_image_url":"https://example.com/a","reference_image_urls":null}]}`, 1, false},
		{`{"elements":[]}`, 0, false},
		{`{"elements":[{"frontal_image_url":"http://127.0.0.1/private"}]}`, 0, true},
		{`{"elements":[{"reference_image_urls":["https://example.com/a","http://192.168.1.1/private"]}]}`, 0, true},
		{`{"elements":[{"frontal_image_url":"file:///etc/passwd"}]}`, 0, true},
		{`{"elements":[{"frontal_image_url":"http://user:pass@example.com/a"}]}`, 0, true},
		{`{"elements":[{"reference_image_urls":"https://example.com/a"}]}`, 0, true},
		{`{"elements":"wrong"}`, 0, true},
	} {
		var body map[string]any
		if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
			t.Fatal(err)
		}
		before, _ := json.Marshal(body)
		count, err := resolveNativeImages(paths, schema, body)
		if (err != nil) != tc.bad || (!tc.bad && count != tc.count) {
			t.Fatalf("count=%d error=%v expected=%+v", count, err, tc)
		}
		after, _ := json.Marshal(body)
		if string(before) != string(after) {
			t.Fatal("native payload modified")
		}
	}
	for _, bad := range [][][]string{paths[:1], {paths[0], paths[0]}, {{"callback_url"}, paths[1]}} {
		if _, err := resolveNativeImages(bad, schema, map[string]any{}); err == nil {
			t.Fatal("unbound or duplicate path accepted")
		}
	}
}

func TestNativeMaskArray(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"reference_mask_urls": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}}
	paths := [][]string{{"reference_mask_urls", "*"}}
	count, err := resolveNativeImages(paths, schema, map[string]any{"reference_mask_urls": []any{"https://example.com/mask.png"}})
	if err != nil || count != 1 {
		t.Fatalf("mask not counted: %d %v", count, err)
	}
	if _, err = resolveNativeImages(paths, schema, map[string]any{"reference_mask_urls": []any{"http://localhost/mask.png"}}); err == nil {
		t.Fatal("unsafe mask URL accepted")
	}
}

func TestNativeControlAndTryOn(t *testing.T) {
	var schema, body map[string]any
	json.Unmarshal([]byte(`{"type":"object","properties":{"controlnet":{"type":"object","properties":{"control_image_url":{"type":"string"}}},"person_image_url":{"type":"string"},"garment_image_urls":{"type":"array","items":{"type":"string"},"maxItems":3}}}`), &schema)
	json.Unmarshal([]byte(`{"controlnet":{"control_image_url":"https://example.com/control"},"person_image_url":"https://example.com/person","garment_image_urls":["https://example.com/garment"]}`), &body)
	paths := [][]string{{"controlnet", "control_image_url"}, {"person_image_url"}, {"garment_image_urls", "*"}}
	count, err := resolveNativeImages(paths, schema, body)
	if err != nil || count != 3 {
		t.Fatalf("count=%d error=%v", count, err)
	}
	body["controlnet"] = map[string]any{"control_image_url": "http://127.0.0.1/private"}
	if _, err = resolveNativeImages(paths, schema, body); err == nil {
		t.Fatal("unsafe ControlNet URL accepted")
	}
}

func TestExtendedNativePathsAreVersionedAndRejectUnsafeValues(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"fill_image":{"type":"object","properties":{"fill_image_url":{"type":"string"}}},"product_image_urls":{"type":"array","items":{"type":"string"}}}}`), &schema); err != nil {
		t.Fatal(err)
	}
	paths := [][]string{{"fill_image", "fill_image_url"}, {"product_image_urls", "*"}}
	legacy, err := nativeImagePaths(schema, 6)
	if err != nil || len(legacy) != 0 {
		t.Fatalf("v6 reinterpreted: %v %v", legacy, err)
	}
	if _, err = resolveNativeImages(paths, schema, map[string]any{}, 6); err == nil {
		t.Fatal("v7 paths accepted under v6")
	}
	if _, err = resolveNativeImages(nil, schema, map[string]any{}, 7); err == nil {
		t.Fatal("missing v7 declarations accepted")
	}
	for _, target := range []string{"https://example.com/a.png", "http://127.0.0.1/a", "http://localhost/a", "file:///etc/passwd", "https://user:secret@example.com/a"} {
		body := map[string]any{"fill_image": map[string]any{"fill_image_url": target}, "product_image_urls": []any{"https://example.com/product.png"}}
		before, _ := json.Marshal(body)
		count, err := resolveNativeImages(paths, schema, body, 7)
		if target == "https://example.com/a.png" {
			if err != nil || count != 2 {
				t.Fatalf("%d %v", count, err)
			}
		} else if err == nil {
			t.Fatalf("unsafe image accepted: %s", target)
		}
		after, _ := json.Marshal(body)
		if string(before) != string(after) {
			t.Fatal("payload changed")
		}
	}
}
