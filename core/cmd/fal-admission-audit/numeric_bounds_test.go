package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestOfficialBriaNumericAliasBoundsSurviveGateway(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/fal-bria-numeric-bounds.json")
	if err != nil {
		t.Fatal(err)
	}
	var original sample
	if err = json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		value any
		pass  bool
	}{{3, true}, {3.5, true}, {5, true}, {2.99, false}, {5.01, false}, {"4", false}, {nil, false}} {
		var request map[string]any
		if err = json.Unmarshal(original.Request, &request); err != nil {
			t.Fatal(err)
		}
		request["guidance_scale"] = tc.value
		changed := original
		changed.Request, _ = json.Marshal(request)
		result := run(changed)
		if result.Passed != tc.pass {
			t.Fatalf("value=%v pass=%v result=%+v", tc.value, tc.pass, result)
		}
		if !tc.pass && result.Stage != "public_validation" {
			t.Fatalf("invalid value reached provider: %+v", result)
		}
	}
}
