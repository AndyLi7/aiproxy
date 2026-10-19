package imagecapabilities_test

import (
	"testing"

	"github.com/labring/aiproxy/core/common/imagecapabilities"
)

func TestReviewedCapabilities(t *testing.T) {
	for _, adapter := range []string{"unknown", "wavespeed", "volcengine-ark-image"} {
		if imagecapabilities.SupportsMeasuredBilling(adapter) ||
			imagecapabilities.SupportsMetering(adapter, "async", 1) {
			t.Fatalf("unexpected measured billing support: %s", adapter)
		}
	}

	for version := 1; version <= 12; version++ {
		if !imagecapabilities.SupportsMetering("fal-image", "async", version) {
			t.Fatalf("lost fal metering version %d", version)
		}
	}

	if !imagecapabilities.SupportsMeasuredBilling("fal-image") ||
		imagecapabilities.SupportsMetering("fal-image", "sync", 1) ||
		imagecapabilities.SupportsMetering("fal-image", "async", 13) {
		t.Fatal("execution or metering version boundary changed")
	}

	ark, ok := imagecapabilities.Lookup("volcengine-ark-image")
	if !ok || ark.Execution != "sync" {
		t.Fatal("lost Ark sync support")
	}

	if _, ok := imagecapabilities.Lookup("wavespeed"); ok {
		t.Fatal("unknown adapter accepted")
	}
}

func TestLookupDoesNotExposeMutableRegistration(t *testing.T) {
	c, _ := imagecapabilities.Lookup("fal-image")
	c.MeteringVersions[0] = 99

	if !imagecapabilities.SupportsMetering("fal-image", "async", 1) ||
		imagecapabilities.SupportsMetering("fal-image", "async", 99) {
		t.Fatal("caller mutated global capabilities")
	}
}
