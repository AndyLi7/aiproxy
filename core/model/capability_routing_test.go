package model

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func capabilityRoutingConfig(
	internalModel string,
	publicModel string,
	capability string,
	required ...string,
) ModelConfig {
	properties := make(map[string]any, len(required))
	for _, name := range required {
		properties[name] = map[string]any{"type": "string"}
	}

	return ModelConfig{
		Model: internalModel,
		Config: map[ModelConfigKey]any{
			ModelConfigCapabilityContractVersionKey: 1,
			ModelConfigPublicModelKey:               publicModel,
			ModelConfigPublicCapabilityModelKey:     publicModel + "/" + capability,
			ModelConfigCapabilityKey:                capability,
			ModelConfigParameterSchemaKey: map[string]any{
				"type":                 "object",
				"properties":           properties,
				"required":             required,
				"additionalProperties": false,
			},
			ModelConfigDefaultParametersKey: map[string]any{},
		},
	}
}

func TestCapabilityRoutingResolvesBaseAndExplicitModels(t *testing.T) {
	configs := []ModelConfig{
		capabilityRoutingConfig(
			"bytedance/seedance-1-0-pro::text-to-video",
			"bytedance/seedance-1-0-pro",
			"text-to-video",
			"prompt",
		),
		capabilityRoutingConfig(
			"bytedance/seedance-1-0-pro::image-to-video",
			"bytedance/seedance-1-0-pro",
			"image-to-video",
			"prompt",
			"first_frame_url",
		),
	}

	tests := []struct {
		name         string
		requested    string
		fields       map[string]any
		wantInternal string
		wantPublic   string
		wantAbility  string
	}{
		{
			name:         "base text request selects text capability",
			requested:    "bytedance/seedance-1-0-pro",
			fields:       map[string]any{"prompt": "sunrise"},
			wantInternal: "bytedance/seedance-1-0-pro::text-to-video",
			wantPublic:   "bytedance/seedance-1-0-pro/text-to-video",
			wantAbility:  "text-to-video",
		},
		{
			name:      "base request with first frame selects more specific capability",
			requested: "bytedance/seedance-1-0-pro",
			fields: map[string]any{
				"prompt":          "animate",
				"first_frame_url": "https://example.com/a.png",
			},
			wantInternal: "bytedance/seedance-1-0-pro::image-to-video",
			wantPublic:   "bytedance/seedance-1-0-pro/image-to-video",
			wantAbility:  "image-to-video",
		},
		{
			name:      "full capability id is deterministic",
			requested: "bytedance/seedance-1-0-pro/image-to-video",
			fields: map[string]any{
				"prompt":          "animate",
				"first_frame_url": "https://example.com/a.png",
			},
			wantInternal: "bytedance/seedance-1-0-pro::image-to-video",
			wantPublic:   "bytedance/seedance-1-0-pro/image-to-video",
			wantAbility:  "image-to-video",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolved, err := ResolveCapabilityModel(test.requested, test.fields, configs)
			require.NoError(t, err)
			require.Equal(t, test.requested, resolved.RequestedModel)
			require.Equal(t, "bytedance/seedance-1-0-pro", resolved.PublicModel)
			require.Equal(t, test.wantPublic, resolved.PublicCapabilityModel)
			require.Equal(t, test.wantAbility, resolved.Capability)
			require.Equal(t, test.wantInternal, resolved.InternalModel)
		})
	}
}

func TestCapabilityRoutingReturnsStableErrors(t *testing.T) {
	text := capabilityRoutingConfig(
		"vendor/model::text-to-video",
		"vendor/model",
		"text-to-video",
		"prompt",
	)
	image := capabilityRoutingConfig(
		"vendor/model::image-to-video",
		"vendor/model",
		"image-to-video",
		"prompt",
		"first_frame_url",
	)

	tests := []struct {
		name      string
		requested string
		fields    map[string]any
		configs   []ModelConfig
		wantCode  CapabilityRoutingErrorCode
	}{
		{
			name:      "unknown model",
			requested: "vendor/missing",
			fields:    map[string]any{"prompt": "hello"},
			configs:   []ModelConfig{text, image},
			wantCode:  CapabilityModelNotFound,
		},
		{
			name:      "base model has no matching capability",
			requested: "vendor/model",
			fields:    map[string]any{},
			configs:   []ModelConfig{text, image},
			wantCode:  CapabilityNotMatched,
		},
		{
			name:      "explicit capability rejects missing required input",
			requested: "vendor/model/image-to-video",
			fields:    map[string]any{"prompt": "hello"},
			configs:   []ModelConfig{text, image},
			wantCode:  CapabilityParameterMismatch,
		},
		{
			name:      "equally specific base candidates are ambiguous",
			requested: "vendor/model",
			fields:    map[string]any{"prompt": "hello"},
			configs: []ModelConfig{
				text,
				capabilityRoutingConfig(
					"vendor/model::prompt-to-video",
					"vendor/model",
					"prompt-to-video",
					"prompt",
				),
			},
			wantCode: CapabilityAmbiguous,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolveCapabilityModel(test.requested, test.fields, test.configs)
			var routingError *CapabilityRoutingError
			require.Error(t, err)
			require.True(t, errors.As(err, &routingError))
			require.Equal(t, test.wantCode, routingError.Code)
			require.NotContains(t, routingError.Error(), "::")
		})
	}
}

func TestCapabilityRoutingResolvesNestedImageGenerationParameters(t *testing.T) {
	const publicModel = "bytedance/seedream-5.0-lite"
	const internalModel = publicModel + "::text-to-image"
	config := capabilityRoutingConfig(
		internalModel,
		publicModel,
		"text-to-image",
		"prompt",
	)
	config.Config[ModelConfigParameterSchemaKey] = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt": map[string]any{"type": "string"},
			"size": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string", "enum": []any{"2K", "3K", "4K"}},
					map[string]any{"type": "string"},
				},
			},
			"sequential_image_generation_options": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"max_images": map[string]any{"type": "integer"},
				},
				"required": []any{"max_images"},
			},
		},
		"required": []any{"prompt"},
	}

	resolved, err := ResolveCapabilityModel(
		publicModel+"/text-to-image",
		map[string]any{
			"prompt": "A dog eating ramen",
			"size":   "2K",
			"sequential_image_generation_options": map[string]any{
				"max_images": float64(1),
			},
		},
		[]ModelConfig{config},
	)

	require.NoError(t, err)
	require.Equal(t, internalModel, resolved.InternalModel)
	require.Equal(t, "text-to-image", resolved.Capability)
}
