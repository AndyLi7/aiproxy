package registryvalidation

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPublicSchemaErrors(t *testing.T) {
	contract := []byte(`{"entry_id":"vendor/image","validation_version":1,"input_schema":{"type":"object","properties":{"size":{"type":"string","enum":["square","portrait"]},"prompt":{"type":"string","minLength":1}},"required":["prompt"],"additionalProperties":false}}`)
	for _, tc := range []struct{ body, param, key string }{
		{`{"model":"vendor/image","prompt":"private prompt","size":"private invalid value"}`, "size", "enum"},
		{`{"model":"vendor/image"}`, "prompt", "minLength"},
		{`{"model":"vendor/image","prompt":""}`, "prompt", "minLength"},
	} {
		_, err := ValidateImage(contract, "vendor/image", []byte(tc.body))
		require.NotNil(t, err)
		require.Equal(t, 400, err.Status)
		require.Equal(t, tc.param, err.Param)
		require.Contains(t, err.Expected, tc.key)
		encoded, e := json.Marshal(err.PublicError())
		require.NoError(t, e)
		require.NotContains(t, string(encoded), "private")
		require.NotContains(t, string(encoded), "registry.invalid")
		require.Contains(t, string(encoded), "invalid_request_error")
	}
}

func TestPublicDimensionErrorReportsPixelBounds(t *testing.T) {
	contract := []byte(`{"entry_id":"vendor/image","validation_version":1,"input_schema":{"type":"object","properties":{"size":{"type":"string"}},"x-image-constraints":{"version":1,"dimensions":{"presets":["square"],"minPixels":921600,"maxPixels":4194304,"minRatio":0.25,"maxRatio":4}}}}`)
	_, err := ValidateImage(contract, "vendor/image", []byte(`{"model":"vendor/image","size":"512x512"}`))
	require.NotNil(t, err)
	require.Equal(t, "size", err.Param)
	require.Equal(t, float64(921600), err.Expected["min_pixels"])
	require.Equal(t, "WIDTHxHEIGHT", err.Expected["custom_format"])
}

func TestRequiredPromptRejectsBlankLegacyContract(t *testing.T) {
	contract := []byte(`{"entry_id":"vendor/image/text-to-image","validation_version":1,"input_schema":{"type":"object","properties":{"prompt":{"type":"string"}},"required":["prompt"],"additionalProperties":false}}`)
	for _, prompt := range []string{"", "  \t"} {
		body, marshalErr := json.Marshal(map[string]string{"model": "vendor/image/text-to-image", "prompt": prompt})
		require.NoError(t, marshalErr)
		_, err := ValidateImage(contract, "vendor/image/text-to-image", body)
		require.NotNil(t, err)
		require.Equal(t, 400, err.Status)
		require.Equal(t, "prompt", err.Param)
		require.Equal(t, 1, err.Expected["minLength"])
	}
	_, err := ValidateImage(contract, "vendor/image/text-to-image", []byte(`{"model":"vendor/image/text-to-image","prompt":"A lighthouse"}`))
	require.Nil(t, err)
}

func TestUnknownImageParameterExplainsUnsupportedField(t *testing.T) {
	contract := []byte(`{"entry_id":"vendor/image","validation_version":1,"input_schema":{"type":"object","properties":{"prompt":{"type":"string"}},"required":["prompt"],"additionalProperties":false}}`)
	_, err := ValidateImage(contract, "vendor/image", []byte(`{"model":"vendor/image","prompt":"test","size":"2K"}`))
	require.NotNil(t, err)
	require.Equal(t, "size", err.Param)
	require.Equal(t, "This parameter is not supported by the selected model.", err.Error())
	require.Equal(t, false, err.Expected["allowed"])
}
