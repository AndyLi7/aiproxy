package model

import "testing"

func TestResolveImageCapabilityRoute(t *testing.T) {
	for _, capability := range []string{"text-to-image", "edit"} {
		publicID := "bytedance/seedream-5.0-lite/" + capability
		route := "bytedance/seedream-5.0-lite::" + capability
		config := map[ModelConfigKey]any{
			"capability_contract_version": 1,
			"public_model":                "bytedance/seedream-5.0-lite",
			"public_capability_model":     publicID,
			"capability":                  capability,
		}
		lookup := func(key string) (map[ModelConfigKey]any, bool) { return config, key == route }
		got, cap := ResolveImageCapabilityRoute(publicID, lookup)
		if got != route || cap != capability {
			t.Fatalf("got %q, %q", got, cap)
		}
		config["public_capability_model"] = "different/model"
		got, cap = ResolveImageCapabilityRoute(publicID, lookup)
		if got != publicID || cap != "" {
			t.Fatal("mismatched metadata must not resolve")
		}
	}
	for _, id := range []string{"dall-e-3", "vendor/model/text-to-video", "internal::edit", "vendor/model/unknown"} {
		got, cap := ResolveImageCapabilityRoute(id, func(string) (map[ModelConfigKey]any, bool) { return nil, false })
		if got != id || cap != "" {
			t.Fatalf("legacy/unknown ID changed: %q", id)
		}
	}
}
