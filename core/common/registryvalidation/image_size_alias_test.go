package registryvalidation

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProductionWanSizeAlias(t *testing.T) {
	contract := []byte(`{"entry_id":"alibaba/2.2-5b/text-to-image","validation_version":1,"input_schema":{"type":"object","properties":{"prompt":{"type":"string","minLength":1},"image_size":{"anyOf":[{"type":"object","properties":{"width":{"type":"integer","minimum":1,"maximum":14142},"height":{"type":"integer","minimum":1,"maximum":14142}},"required":["width","height"],"additionalProperties":false},{"type":"string","enum":["square_hd","landscape_16_9"]}],"default":"square_hd"}},"required":["prompt"],"additionalProperties":false}}`)
	normalized, validationErr := ValidateImage(contract, "alibaba/2.2-5b/text-to-image", []byte(`{"model":"alibaba/2.2-5b/text-to-image","prompt":"lion","size":"1536x1024"}`))
	require.Nil(t, validationErr)
	var result map[string]any
	require.NoError(t, json.Unmarshal(normalized, &result))
	require.Equal(t, map[string]any{"width": float64(1536), "height": float64(1024)}, result["image_size"])
	require.NotContains(t, result, "size")
}

func TestImageClientDefaultsAndSizeExclusion(t *testing.T) {
	contract := []byte(`{"entry_id":"test/wan/text-to-image","validation_version":1,"input_schema":{"type":"object","properties":{"prompt":{"type":"string","minLength":1,"maxLength":2000},"negative_prompt":{"type":"string","maxLength":500,"default":""},"n":{"type":"integer","minimum":1,"maximum":5,"default":1},"seed":{"type":"integer","minimum":0,"maximum":2147483647},"image_size":{"type":"string","enum":["square_hd"]}},"required":["prompt","negative_prompt","n"],"additionalProperties":false}}`)
	normalized, err := ValidateImage(contract, "test/wan/text-to-image", []byte(`{"model":"test/wan/text-to-image","prompt":"fox"}`))
	require.Nil(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(normalized, &body))
	require.Equal(t, "", body["negative_prompt"])
	require.Equal(t, float64(1), body["n"])
	for _, input := range []string{
		`{"model":"test/wan/text-to-image","prompt":"fox","size":"square_hd","image_size":"square_hd"}`,
		`{"model":"test/wan/text-to-image","prompt":"fox","seed":-1}`,
		`{"model":"test/wan/text-to-image","prompt":"fox","seed":2147483648}`,
		`{"model":"test/wan/text-to-image","prompt":"fox","n":null}`,
	} {
		_, failure := ValidateImage(contract, "test/wan/text-to-image", []byte(input))
		require.NotNil(t, failure, input)
	}
}
