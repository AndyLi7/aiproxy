package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestOfficialLogoOverlayKeepsConstraintsAndNestedURLGuard(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/pixelcut-logo-overlay.json")
	if err != nil {
		t.Fatal(err)
	}
	var s sample
	if err = json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		watermark any
		pass      bool
	}{
		{nil, true},
		{map[string]any{"image_url": "https://example.com/logo.png", "position": "bottom_right", "opacity": 0.8, "scale": 0.1, "remove_background": true, "margin": "3%"}, true},
		{map[string]any{"image_url": "https://example.com/logo.png", "opacity": 1.1}, false},
		{map[string]any{"image_url": "https://example.com/logo.png", "scale": 0}, false},
		{map[string]any{"image_url": "https://example.com/logo.png", "position": "unknown"}, false},
		{map[string]any{"image_url": "http://127.0.0.1/logo.png"}, false},
		{map[string]any{"image_url": "https://user:pass@example.com/logo.png"}, false},
		{true, false},
	}
	for _, tc := range tests {
		var request map[string]any
		if err = json.Unmarshal(s.Request, &request); err != nil {
			t.Fatal(err)
		}
		request["watermark"] = tc.watermark
		changed := s
		changed.Request, _ = json.Marshal(request)
		result := run(changed)
		if result.Passed != tc.pass {
			t.Fatalf("watermark=%v result=%+v", tc.watermark, result)
		}
	}
}
