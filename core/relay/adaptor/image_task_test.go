package adaptor_test

import (
	"encoding/json"
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/stretchr/testify/require"
)

func TestImageProviderMappingIsRegistryDriven(t *testing.T) {
	config := map[model.ModelConfigKey]any{
		"x_token_platform_capability_contract": map[string]any{
			"contract": map[string]any{
				"providers": map[string]any{
					"custom-provider": map[string]any{
						"adapter": "fal-image",
						"parameterMapping": map[string]string{
							"n":       "num_images",
							"quality": "quality_mode",
						},
						"allowedPassthroughParameters": []string{"prompt"},
						"fixedParameters":              map[string]any{"format": "png"},
					},
				},
			},
		},
	}
	mapped, err := adaptor.MapImageProviderInput(
		config,
		"fal-image",
		[]byte(`{"model":"public","n":1,"quality":"high","prompt":"hi"}`),
	)
	require.NoError(t, err)

	var input map[string]any
	require.NoError(t, json.Unmarshal(mapped, &input))
	require.Equal(
		t,
		map[string]any{
			"num_images":   float64(1),
			"quality_mode": "high",
			"prompt":       "hi",
			"format":       "png",
		},
		input,
	)

	_, err = adaptor.MapImageProviderInput(
		config,
		"fal-image",
		[]byte(`{"model":"public","unknown":true}`),
	)
	require.Error(t, err)
	_, err = adaptor.MapImageProviderInput(
		config,
		"fal-image",
		[]byte(`{"model":"public","format":"jpg"}`),
	)
	require.Error(t, err)
}
