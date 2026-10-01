package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestOfficialStepReflectionObjectArrayReplay(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/stepx-reflection-metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	var s sample
	if err = json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		value any
		pass  bool
	}{{nil, true}, {[]any{}, true}, {[]any{map[string]any{"score": 0.9, "notes": []any{"refinement"}, "nested": map[string]any{"preserved": true}}}, true}, {map[string]any{}, false}, {[]any{3}, false}} {
		var response map[string]any
		if err = json.Unmarshal(s.Response, &response); err != nil {
			t.Fatal(err)
		}
		response["best_info"] = tc.value
		changed := s
		changed.Response, _ = json.Marshal(response)
		result := run(changed)
		if !result.Passed || result.OutputPassed != tc.pass {
			t.Fatalf("value=%v result=%+v", tc.value, result)
		}
	}
}
