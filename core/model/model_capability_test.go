package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelCapabilityKeyRoundTrip(t *testing.T) {
	t.Parallel()

	require.Equal(t, 1, ModelCapabilityContractVersion)

	key, err := BuildModelCapabilityKey(
		"bytedance/seedance-2.0",
		ModelCapabilityTextToVideo,
	)
	require.NoError(t, err)
	require.Equal(t, "bytedance/seedance-2.0::text-to-video", key)

	modelName, capability, ok := ParseModelCapabilityKey(key)
	require.True(t, ok)
	require.Equal(t, "bytedance/seedance-2.0", modelName)
	require.Equal(t, ModelCapabilityTextToVideo, capability)
}

func TestModelCapabilityValidationRejectsUnknownAndReservedValues(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		modelName  string
		capability ModelCapability
	}{
		{name: "unknown capability", modelName: "bytedance/seedance-2.0", capability: "video-to-video"},
		{name: "reserved model separator", modelName: "bytedance/seedance::2.0", capability: ModelCapabilityTextToVideo},
		{name: "reserved capability separator", modelName: "bytedance/seedance-2.0", capability: "text::video"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := BuildModelCapabilityKey(tc.modelName, tc.capability)
			require.Error(t, err)
		})
	}

	_, _, ok := ParseModelCapabilityKey("bytedance/seedance-2.0::video-to-video")
	require.False(t, ok)
}

func TestValidateModelCapabilityConfig(t *testing.T) {
	t.Parallel()

	valid := map[ModelConfigKey]any{
		"capability_contract_version": float64(ModelCapabilityContractVersion),
		"public_model":                "bytedance/seedance-2.0",
		"capability":                  "text-to-video",
	}
	require.NoError(t, ValidateModelCapabilityConfig(
		valid,
		"bytedance/seedance-2.0",
		ModelCapabilityTextToVideo,
	))

	for _, config := range []map[ModelConfigKey]any{
		{
			"capability_contract_version": float64(2),
			"public_model":                "bytedance/seedance-2.0",
			"capability":                  "text-to-video",
		},
		{
			"capability_contract_version": float64(ModelCapabilityContractVersion),
			"public_model":                "bytedance/other-model",
			"capability":                  "text-to-video",
		},
		{
			"capability_contract_version": float64(ModelCapabilityContractVersion),
			"public_model":                "bytedance/seedance-2.0",
			"capability":                  "image-to-video",
		},
	} {
		require.Error(t, ValidateModelCapabilityConfig(
			config,
			"bytedance/seedance-2.0",
			ModelCapabilityTextToVideo,
		))
	}
}
