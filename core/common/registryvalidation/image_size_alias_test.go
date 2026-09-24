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
