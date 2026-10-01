package registryvalidation

import (
	"encoding/json"
	"os"
	"testing"
)

func TestRealEmptySchemaArrayItems(t *testing.T) {
	data, err := os.ReadFile("testdata/provider-empty-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Endpoint string
		Schema   map[string]any
	}
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 4 {
		t.Fatal("missing sources")
	}
	for _, f := range fixtures {
		for _, value := range []any{nil, []any{}, []any{nil, "text", 42.0, true, map[string]any{"text": "example", "position": []any{1.0, 2.0}}}} {
			if err := schemaAccepts(f.Schema, value); err != nil {
				t.Fatalf("%s rejected JSON items: %v", f.Endpoint, err)
			}
		}
		for _, value := range []any{"not-an-array", true, 1.0, map[string]any{}} {
			if err := schemaAccepts(f.Schema, value); err == nil {
				t.Fatalf("%s lost outer constraint", f.Endpoint)
			}
		}
	}
}
