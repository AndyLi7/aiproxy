package registryvalidation

import (
	"encoding/json"
	"os"
	"testing"
)

func TestSeedream45ArkSingleProjection(t *testing.T) {
	raw, err := os.ReadFile("testdata/seedream45-task.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Providers map[string]struct {
			Upstream ProviderSpec `json:"upstream"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	s := contract.Providers["volcengine"].Upstream
	b := ProviderBinding{Provider: "volcengine", ID: s.ID, Revision: s.Revision, ContractHash: s.ContractHash}
	for _, tc := range []struct{ size, expected string }{
		{"auto_2K", "2K"}, {"auto_4K", "4K"},
	} {
		body, _ := json.Marshal(map[string]any{"model": "bytedance/seedream-4.5/text-to-image-v2", "prompt": "cover", "n": 1, "image_size": tc.size, "max_images": 1})
		mapped, err := MapBoundProviderInput(raw, b, "volcengine-ark-image", s.Endpoint, "sync", body)
		if err != nil {
			t.Fatal(err)
		}
		var native map[string]any
		if err := json.Unmarshal(mapped, &native); err != nil {
			t.Fatal(err)
		}
		if native["size"] != tc.expected || native["prompt"] != "cover" || native["stream"] != false || native["response_format"] != "url" || len(native) != 4 {
			t.Fatalf("unsafe native projection: %v", native)
		}
		metering, err := ResolveImageMetering(raw, b, mapped, 1, 1024, false)
		if err != nil || metering.MaximumOutputs != 1 {
			t.Fatalf("metering: %+v %v", metering, err)
		}
	}
	for _, bad := range []map[string]any{
		{"prompt": "cover", "n": 2, "image_size": "auto_2K", "max_images": 1},
		{"prompt": "cover", "n": 1, "image_size": "auto_2K", "max_images": 2},
		{"prompt": "cover", "n": 1, "image_size": "square_hd", "max_images": 1},
		{"prompt": "cover", "n": 1, "image_size": "auto_2K", "max_images": 1, "seed": 42},
	} {
		body, _ := json.Marshal(bad)
		if _, err := MapBoundProviderInput(raw, b, "volcengine-ark-image", s.Endpoint, "sync", body); err == nil {
			t.Fatalf("accepted incompatible Ark request: %v", bad)
		}
	}
}
