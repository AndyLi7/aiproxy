package model_test

import (
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

func nativeConfig(route, publicID string, m mode.Mode) model.ModelConfig {
	return model.ModelConfig{
		Model:  route,
		Type:   m,
		Config: map[model.ModelConfigKey]any{model.ModelConfigPublicCapabilityModelKey: publicID},
	}
}

func TestResolveNativeCapabilityRoute(t *testing.T) {
	configs := map[string]model.ModelConfig{
		"vendor/flux::text-to-image": nativeConfig("vendor/flux::text-to-image", "vendor/flux/text-to-image", mode.NativeTasks),
		"vendor/img::text-to-image":  nativeConfig("vendor/img::text-to-image", "vendor/img/text-to-image", mode.ImagesGenerations),
	}

	require.Equal(t, "vendor/flux::text-to-image",
		model.ResolveNativeCapabilityRoute("vendor/flux/text-to-image", configs))
	// Only native configs are eligible; image capabilities keep their own resolver.
	require.Equal(t, "vendor/img/text-to-image",
		model.ResolveNativeCapabilityRoute("vendor/img/text-to-image", configs))
	require.Equal(t, "vendor/unknown/text-to-image",
		model.ResolveNativeCapabilityRoute("vendor/unknown/text-to-image", configs))

	// Two native configs claiming the same public id are never guessed between.
	configs["vendor/flux::copy"] = nativeConfig("vendor/flux::copy", "vendor/flux/text-to-image", mode.NativeTasks)
	require.Equal(t, "vendor/flux/text-to-image",
		model.ResolveNativeCapabilityRoute("vendor/flux/text-to-image", configs))
}
