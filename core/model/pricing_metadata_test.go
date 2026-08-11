package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelConfigRetailPricingMetadata(t *testing.T) {
	config := ModelConfig{Config: map[ModelConfigKey]any{
		ModelConfigKey("x_token_platform_pricing"): map[string]any{
			"currency":        "USD",
			"pricing_version": "21",
		},
	}}

	currency, version, ok := config.RetailPricingMetadata()
	require.True(t, ok)
	require.Equal(t, "USD", currency)
	require.Equal(t, "21", version)
}

func TestModelConfigRetailPricingMetadataRejectsIncompleteValues(t *testing.T) {
	config := ModelConfig{Config: map[ModelConfigKey]any{
		ModelConfigKey("x_token_platform_pricing"): map[string]any{
			"currency": "USD",
		},
	}}

	_, _, ok := config.RetailPricingMetadata()
	require.False(t, ok)
}
