package registryvalidation

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

// Fixture exported by compileCapabilityDefinition(seedream50LiteEdit), not handwritten.
func TestCompiledSeedreamContract(t *testing.T) {
	contract, err := os.ReadFile("testdata/seedream-edit.json")
	require.NoError(t, err)
	for _, tc := range []struct {
		size  string
		valid bool
	}{{"3750x1250", true}, {"2K", true}, {"1500x1500", false}, {"8192x8192", false}} {
		body, _ := json.Marshal(map[string]any{"model": "bytedance/seedream-5.0-lite/edit", "prompt": "A cup", "image": []string{"https://example.com/a.png"}, "size": tc.size})
		_, validationErr := ValidateImage(contract, "bytedance/seedream-5.0-lite/edit", body)
		if tc.valid {
			require.Nil(t, validationErr, tc.size)
		} else {
			require.NotNil(t, validationErr, tc.size)
			require.Equal(t, 400, validationErr.Status)
		}
	}
}

func TestCompiledSeedreamTransportParameters(t *testing.T) {
	contract, err := os.ReadFile("testdata/seedream-edit.json")
	require.NoError(t, err)
	for _, stream := range []bool{false, true} {
		body, _ := json.Marshal(map[string]any{"model": "bytedance/seedream-5.0-lite/edit", "prompt": "A cup", "image": "https://example.com/a.png", "response_format": "url", "stream": stream})
		normalized, validationErr := ValidateImage(contract, "bytedance/seedream-5.0-lite/edit", body)
		if stream {
			require.NotNil(t, validationErr)
			require.Equal(t, 400, validationErr.Status)
			continue
		}
		require.Nil(t, validationErr)
		var forwarded map[string]any
		require.NoError(t, json.Unmarshal(normalized, &forwarded))
		require.Equal(t, false, forwarded["stream"])
		require.Equal(t, "url", forwarded["response_format"])
	}
}

func testContract() []byte {
	return []byte(`{"entry_id":"vendor/image/edit","validation_version":1,"input_schema":{"type":"object","properties":{"prompt":{"type":"string","minLength":1},"size":{"type":"string","default":"2K"},"image":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":14},"sequential_image_generation":{"type":"string","enum":["auto","disabled"],"default":"disabled"},"sequential_image_generation_options":{"type":"object","properties":{"max_images":{"type":"integer","minimum":1,"maximum":15}},"required":["max_images"],"additionalProperties":false,"default":{"max_images":1}}},"required":["prompt","size","image","sequential_image_generation","sequential_image_generation_options"],"additionalProperties":false,"x-image-constraints":{"version":1,"dimensions":{"presets":["2K","3K","4K"],"minPixels":3686400,"maxPixels":16777216,"minRatio":0.0625,"maxRatio":16},"combinedImages":{"maximum":15}}}}`)
}

func TestValidateImageBoundariesAndDefaults(t *testing.T) {
	for _, size := range []string{"1500x1500", "0x0", "8192x8192", "32000x128"} {
		_, err := ValidateImage(testContract(), "vendor/image/edit", []byte(`{"model":"vendor/image/edit","prompt":"cup","image":["https://example.com/a.png"],"size":"`+size+`"}`))
		require.Error(t, err, size)
		require.Equal(t, 400, err.Status)
	}
	body, err := ValidateImage(testContract(), "vendor/image/edit", []byte(`{"model":"vendor/image/edit","prompt":"cup","image":["https://example.com/a.png"]}`))
	require.Nil(t, err)
	var output map[string]any
	require.NoError(t, json.Unmarshal(body, &output))
	require.Equal(t, "2K", output["size"])
	require.Equal(t, "vendor/image/edit", output["model"])
}

func TestValidateImageRejectsSchemaAndContractFailures(t *testing.T) {
	for _, body := range []string{`{"model":"vendor/image/edit","prompt":"","image":["x"]}`, `{"model":"vendor/image/edit","prompt":"cup","image":["x"],"extra":1}`} {
		_, err := ValidateImage(testContract(), "vendor/image/edit", []byte(body))
		require.NotNil(t, err)
		require.Equal(t, 400, err.Status)
	}
	for _, contract := range []string{`{}`, `{"entry_id":"vendor/image/edit","validation_version":2}`, `{"entry_id":"vendor/image/edit","validation_version":1,"input_schema":{"$ref":"file:///secret.json"}}`} {
		_, err := ValidateImage([]byte(contract), "vendor/image/edit", []byte(`{}`))
		require.NotNil(t, err)
		require.Equal(t, 503, err.Status)
	}
}

func TestValidateImageCombinedCountAndRegistryChange(t *testing.T) {
	refs := make([]string, 14)
	for i := range refs {
		refs[i] = "https://example.com/a.png"
	}
	body, _ := json.Marshal(map[string]any{"model": "vendor/image/edit", "prompt": "cup", "image": refs, "sequential_image_generation": "auto", "sequential_image_generation_options": map[string]any{"max_images": 2}})
	_, err := ValidateImage(testContract(), "vendor/image/edit", body)
	require.NotNil(t, err)
	require.Equal(t, 400, err.Status)
	var changed map[string]any
	require.NoError(t, json.Unmarshal(testContract(), &changed))
	changed["input_schema"].(map[string]any)["x-image-constraints"].(map[string]any)["combinedImages"].(map[string]any)["maximum"] = 16
	contract, _ := json.Marshal(changed)
	_, err = ValidateImage(contract, "vendor/image/edit", body)
	require.Nil(t, err, "limit must be read from registry, not hardcoded")
}
