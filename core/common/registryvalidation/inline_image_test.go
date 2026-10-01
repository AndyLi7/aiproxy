package registryvalidation

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/image/webp"
)

func tinyInlineImage(t *testing.T, format string) []byte {
	t.Helper()
	var out bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.White)
	if format == "image/webp" {
		data, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
		require.NoError(t, err)
		config, err := webp.DecodeConfig(bytes.NewReader(data))
		require.NoError(t, err)
		require.Equal(t, 1, config.Width)
		require.Equal(t, 1, config.Height)
		return data
	}
	if format == "image/png" {
		require.NoError(t, png.Encode(&out, img))
	} else {
		require.NoError(t, jpeg.Encode(&out, img, nil))
	}
	return out.Bytes()
}
func TestNativeInlineImageEncodingSizeAndMediaPolicy(t *testing.T) {
	for _, mediaType := range []string{"image/png", "image/jpeg", "image/webp"} {
		data := tinyInlineImage(t, mediaType)
		text := base64.StdEncoding.EncodeToString(data)
		policy := NativeInlineImagePolicy{Encoding: "raw-base64", MaxDecodedBytes: int64(len(data)), MediaTypes: []string{mediaType}}
		require.NoError(t, ValidateNativeInlineImage(text, policy))
		// Padding must not cause an exact-size valid image to be rejected.
		policy.MaxDecodedBytes--
		require.Error(t, ValidateNativeInlineImage(text, policy))
		policy.MaxDecodedBytes++
		require.NoError(t, ValidateNativeInlineImage(text[:32]+"\r\n"+text[32:], policy))
		require.Error(t, ValidateNativeInlineImage("data:"+mediaType+";base64,"+text, policy))
		policy.Encoding = "data-uri"
		require.NoError(t, ValidateNativeInlineImage("data:"+mediaType+";base64,"+text, policy))
		require.Error(t, ValidateNativeInlineImage(text, policy))
		require.Error(t, ValidateNativeInlineImage("data:image/gif;base64,"+text, policy))
		policy.MediaTypes = []string{"image/png", "image/jpeg", "image/webp"}
		wrong := "image/jpeg"
		if mediaType == wrong {
			wrong = "image/png"
		}
		require.Error(t, ValidateNativeInlineImage("data:"+wrong+";base64,"+text, policy))
		require.Error(t, ValidateNativeInlineImage("data:"+mediaType+";charset=utf-8;base64,"+text, policy))
	}
}
func TestNativeInlineImageRejectsMalformedAndNonImages(t *testing.T) {
	policy := NativeInlineImagePolicy{Encoding: "raw-base64", MaxDecodedBytes: 1024, MediaTypes: []string{"image/png", "image/jpeg", "image/webp"}}
	for _, text := range []string{"", "https://example.com/a.png", "http://127.0.0.1/a", base64.StdEncoding.EncodeToString([]byte("<svg></svg>")), base64.StdEncoding.EncodeToString([]byte("GIF89a fake")), "AA===", "A!AA", strings.Repeat("A", 2048), strings.Repeat("\n", 2048)} {
		require.Error(t, ValidateNativeInlineImage(text, policy), text[:min(len(text), 30)])
	}
	policy.Encoding = "unknown"
	require.Error(t, ValidateNativeInlineImage("AA==", policy))
	policy.Encoding = "raw-base64"
	policy.MediaTypes = []string{"image/*"}
	require.Error(t, validateInlineImagePolicy(policy))
	policy.MediaTypes = []string{"image/png", "image/png"}
	require.Error(t, validateInlineImagePolicy(policy))
	policy.MediaTypes = []string{"image/png"}
	policy.MaxDecodedBytes = 0
	require.Error(t, validateInlineImagePolicy(policy))
}

func TestInlineImagePolicyIsBoundToDeclaredLeafAndVersion(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"guidance":  map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"image_url": map[string]any{"type": "string"}}}},
		"image_url": map[string]any{"type": "string"},
	}}
	paths, err := nativeImagePaths(schema, 9)
	require.NoError(t, err)
	text := base64.StdEncoding.EncodeToString(tinyInlineImage(t, "image/png"))
	binding := NativeInlineImageBinding{Path: []string{"guidance", "*", "image_url"}, NativeInlineImagePolicy: NativeInlineImagePolicy{Encoding: "raw-base64", MaxDecodedBytes: 1024, MediaTypes: []string{"image/png"}}}
	body := map[string]any{"guidance": []any{map[string]any{"image_url": text}, map[string]any{"image_url": text}}, "image_url": "https://example.com/a.png"}
	count, err := resolveNativeImagesWithInlinePolicy(paths, schema, body, 9, []NativeInlineImageBinding{binding})
	require.NoError(t, err)
	require.Equal(t, int64(3), count)
	require.Equal(t, text, body["guidance"].([]any)[0].(map[string]any)["image_url"])
	_, err = resolveNativeImages(paths, schema, body, 8)
	require.Error(t, err)
	_, err = resolveNativeImagesWithInlinePolicy(paths, schema, body, 8, []NativeInlineImageBinding{binding})
	require.Error(t, err)
	_, err = resolveNativeImagesWithInlinePolicy(paths, schema, body, 9, nil)
	require.Error(t, err)
	_, err = resolveNativeImagesWithInlinePolicy(paths, schema, body, 9, []NativeInlineImageBinding{binding, binding})
	require.Error(t, err)
	unknown := binding
	unknown.Path = []string{"prompt"}
	_, err = resolveNativeImagesWithInlinePolicy(paths, schema, body, 9, []NativeInlineImageBinding{unknown})
	require.Error(t, err)
	body["image_url"] = text
	_, err = resolveNativeImagesWithInlinePolicy(paths, schema, body, 9, []NativeInlineImageBinding{binding})
	require.Error(t, err)
	body["image_url"] = "https://example.com/a.png"
	leaf := body["guidance"].([]any)[0].(map[string]any)
	leaf["image_url"] = "https://example.com/b.png"
	_, err = resolveNativeImagesWithInlinePolicy(paths, schema, body, 9, []NativeInlineImageBinding{binding})
	require.Error(t, err)
	binding.AllowURL = true
	count, err = resolveNativeImagesWithInlinePolicy(paths, schema, body, 9, []NativeInlineImageBinding{binding})
	require.NoError(t, err)
	require.Equal(t, int64(3), count)
	for _, bad := range []string{"http://127.0.0.1/a.png", "http://localhost/a.png", "http://10.0.0.1/a.png", "http://user:pass@example.com/a.png", "data:image/png;base64," + text} {
		leaf["image_url"] = bad
		_, err = resolveNativeImagesWithInlinePolicy(paths, schema, body, 9, []NativeInlineImageBinding{binding})
		require.Error(t, err)
	}
}

func TestUpstreamImageContentValidationPreservesEncodingBoundary(t *testing.T) {
	p := NativeInlineImagePolicy{Encoding: "raw-base64", MaxDecodedBytes: 100, MediaTypeValidation: "upstream", MediaTypes: []string{}}
	// Encoded bytes are forwarded; the provider, not this check, validates image contents.
	require.NoError(t, ValidateNativeInlineImage("aGVsbG8=", p))
	for _, v := range []string{"", "not base64", "data:image/png;base64,aGVsbG8=", "aGVsbG8", "aGVsbG9="} {
		require.Error(t, ValidateNativeInlineImage(v, p))
	}
	p.Encoding = "data-uri"
	require.NoError(t, ValidateNativeInlineImage("data:image/png;base64,aGVsbG8=", p))
	for _, v := range []string{"aGVsbG8=", "data:text/html;base64,aGVsbG8=", "data:image/png;a=1;base64,aGVsbG8=", "data:image/png,aGVsbG8="} {
		require.Error(t, ValidateNativeInlineImage(v, p))
	}
	p.MaxDecodedBytes = 4
	require.Error(t, ValidateNativeInlineImage("data:image/png;base64,aGVsbG8=", p))
	p.MediaTypes = []string{"image/png"}
	require.Error(t, ValidateNativeInlineImage("data:image/png;base64,aGVsbG8=", p))
}

func TestMixedInlineVersionIsolationAndPrivateURLProtection(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"image_url": map[string]any{"type": "string"}}}
	binding := NativeInlineImageBinding{Path: []string{"image_url"}, AllowURL: true, NativeInlineImagePolicy: NativeInlineImagePolicy{Encoding: "raw-base64", MaxDecodedBytes: 100, MediaTypeValidation: "upstream", MediaTypes: []string{}}}
	for _, text := range []string{"aGVsbG8=", "https://example.com/image.png"} {
		body := map[string]any{"image_url": text}
		count, err := resolveNativeImagesWithInlinePolicy([][]string{{"image_url"}}, schema, body, 10, []NativeInlineImageBinding{binding})
		require.NoError(t, err)
		require.EqualValues(t, 1, count)
		_, err = resolveNativeImagesWithInlinePolicy([][]string{{"image_url"}}, schema, body, 9, []NativeInlineImageBinding{binding})
		require.Error(t, err)
	}
	for _, text := range []string{"http://localhost/x", "http://192.168.1.1/x", "file:///private", "data:image/png;base64,aGVsbG8="} {
		_, err := resolveNativeImagesWithInlinePolicy([][]string{{"image_url"}}, schema, map[string]any{"image_url": text}, 10, []NativeInlineImageBinding{binding})
		require.Error(t, err)
	}
}

func TestDocumentedGarmentDataURIRequiresExactBinding(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"model_image": map[string]any{"type": "string", "description": "URL or base64 of the model image"}, "garment_image": map[string]any{"type": "string", "description": "URL or base64 of the garment image"}}}
	paths := [][]string{{"model_image"}, {"garment_image"}}
	bindings := []NativeInlineImageBinding{}
	for _, path := range paths {
		bindings = append(bindings, NativeInlineImageBinding{Path: path, AllowURL: true, NativeInlineImagePolicy: NativeInlineImagePolicy{Encoding: "data-uri", MaxDecodedBytes: 50 * 1024 * 1024, MediaTypes: []string{}, MediaTypeValidation: "upstream"}})
	}
	for _, v := range []string{"https://example.com/image.png", "data:image/png;base64,aGVsbG8="} {
		body := map[string]any{"model_image": v, "garment_image": "https://example.com/garment.webp"}
		n, err := resolveNativeImagesWithInlinePolicy(paths, schema, body, 12, bindings)
		require.NoError(t, err)
		require.EqualValues(t, 2, n)
		require.Equal(t, v, body["model_image"])
	}
	for _, v := range []string{"aGVsbG8=", "data:text/html;base64,aGVsbG8=", "data:image/png;base64,broken!", "https://user:pass@example.com/image.png", "http://127.0.0.1/image.png"} {
		_, err := resolveNativeImagesWithInlinePolicy(paths, schema, map[string]any{"model_image": v, "garment_image": "https://example.com/a.png"}, 12, bindings)
		require.Error(t, err)
	}
	body := map[string]any{"model_image": "https://example.com/a.png", "garment_image": "https://example.com/b.png"}
	for _, mode := range []string{"encoding", "allowUrl", "maximum", "missing"} {
		altered := append([]NativeInlineImageBinding(nil), bindings...)
		switch mode {
		case "encoding":
			altered[0].Encoding = "raw-base64"
		case "allowUrl":
			altered[0].AllowURL = false
		case "maximum":
			altered[0].MaxDecodedBytes = 1
		case "missing":
			altered = altered[:1]
		}
		_, err := resolveNativeImagesWithInlinePolicy(paths, schema, body, 12, altered)
		require.Error(t, err)
	}
	schema["properties"].(map[string]any)["model_image"].(map[string]any)["description"] = "ambiguous image"
	schema["properties"].(map[string]any)["garment_image"].(map[string]any)["description"] = "ambiguous image"
	_, err := resolveNativeImagesWithInlinePolicy(paths, schema, body, 12, bindings)
	require.Error(t, err)
}
