package imagecapabilities

import "testing"

func TestReviewedCapabilities(t *testing.T) {
	for _, adapter := range []string{"unknown", "wavespeed", "volcengine-ark-image"} {
		if SupportsMeasuredBilling(adapter) || SupportsMetering(adapter, "async", 1) {
			t.Fatalf("unexpected measured billing support: %s", adapter)
		}
	}
	for version := 1; version <= 5; version++ {
		if !SupportsMetering("fal-image", "async", version) {
			t.Fatalf("lost fal metering version %d", version)
		}
	}
	if !SupportsMeasuredBilling("fal-image") || SupportsMetering("fal-image", "sync", 1) || SupportsMetering("fal-image", "async", 6) {
		t.Fatal("execution or metering version boundary changed")
	}
	ark, ok := Lookup("volcengine-ark-image")
	if !ok || ark.Execution != "sync" {
		t.Fatal("lost Ark sync support")
	}
	if _, ok := Lookup("wavespeed"); ok {
		t.Fatal("unknown adapter accepted")
	}
}

func TestLookupDoesNotExposeMutableRegistration(t *testing.T) {
	c, _ := Lookup("fal-image")
	c.MeteringVersions[0] = 99
	if !SupportsMetering("fal-image", "async", 1) || SupportsMetering("fal-image", "async", 99) {
		t.Fatal("caller mutated global capabilities")
	}
}
