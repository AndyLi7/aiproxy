//nolint:testpackage // Tests exercise the parser and its bounded image fixtures.
package imagecompat

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStrictGenerationAndIdentity(t *testing.T) {
	parse := func(body string) (Request, error) {
		r := httptest.NewRequestWithContext(
			t.Context(),
			http.MethodPost,
			"/v1/images/generations",
			strings.NewReader(body),
		)
		r.Header.Set("Content-Type", "application/json")

		return Parse(r, false)
	}
	result, err := parse(`{"model":"` + GenerateModel + `","prompt":"hello","size":"1024x1024"}`)
	require.NoError(t, err)
	require.Equal(t, "b64_json", result.Format)

	var body map[string]any
	require.NoError(t, json.Unmarshal(result.Body, &body))
	input, ok := body["input"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "1024x1024", input["image_size"])
	require.Equal(t, float64(1), input["num_images"])
	require.NotContains(t, input, "quality")
	require.NotContains(t, input, "background")

	for _, tail := range []string{`,"n":2`, `,"n":null`, `,"n":1.0`, `,"stream":false`, `,"mask":"x"`, `,"quality":null`, `,"input_fidelity":"high"`, `,"response_format":null`, `,"response_format":"jpeg"`, `,"prompt":"second"`} {
		_, err := parse(`{"model":"` + GenerateModel + `","prompt":"hello"` + tail + `}`)
		require.Error(t, err, tail)
	}

	headers := http.Header{"Idempotency-Key": []string{"stable"}}
	id, err := OperationID(headers, "g", 1)
	require.NoError(t, err)
	headers.Set("X-Request-ID", "edge-generated")
	headers.Set("X-Stainless-Retry-Count", "2")
	same, err := OperationID(headers, "g", 1)
	require.NoError(t, err)
	require.Equal(t, id, same)

	other, _ := OperationID(headers, "g", 2)
	require.NotEqual(t, id, other)
	headers.Del("Idempotency-Key")
	_, err = OperationID(headers, "g", 1)
	require.ErrorIs(t, err, ErrRetry)
	headers.Del("X-Stainless-Retry-Count")
	first, _ := OperationID(headers, "g", 1)
	second, _ := OperationID(headers, "g", 1)
	require.NotEqual(t, first, second)
}

func editRequest(t *testing.T, images [][]byte, names []string) *http.Request {
	t.Helper()

	var body bytes.Buffer

	w := multipart.NewWriter(&body)
	require.NoError(t, w.WriteField("model", EditModel))
	require.NoError(t, w.WriteField("prompt", "edit image"))

	for i, b := range images {
		f, err := w.CreateFormFile(names[i], "untrusted.png")
		require.NoError(t, err)
		_, err = f.Write(b)
		require.NoError(t, err)
	}

	require.NoError(t, w.Close())

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/images/edits", &body)
	r.Header.Set("Content-Type", w.FormDataContentType())

	return r
}

func TestEditValidatesAllFilesAndCompleteImage(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, png.Encode(&out, image.NewRGBA(image.Rect(0, 0, 2, 2))))

	pngBytes := out.Bytes()
	for _, field := range []string{"image", "image[]"} {
		result, err := Parse(editRequest(t, [][]byte{pngBytes}, []string{field}), true)
		require.NoError(t, err)
		require.Contains(t, string(result.Body), "data:image/png;base64,")
	}

	for _, files := range [][][]byte{{pngBytes, pngBytes}, {[]byte("not image")}, {pngBytes[:len(pngBytes)-10]}, {bytes.Repeat([]byte("a"), MaxImageBytes+1)}} {
		names := []string{"image", "image[]"}
		_, err := Parse(editRequest(t, files, names[:len(files)]), true)
		require.Error(t, err)
	}

	_, err := Parse(editRequest(t, [][]byte{pngBytes}, []string{"mask"}), true)
	require.Error(t, err)
}

func TestEditJPEGAndWebP(t *testing.T) {
	var jpegData bytes.Buffer
	require.NoError(t, jpeg.Encode(&jpegData, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil))

	webpData, err := base64.StdEncoding.DecodeString(
		"UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA",
	)
	require.NoError(t, err)

	for _, data := range [][]byte{jpegData.Bytes(), webpData} {
		_, err := Parse(editRequest(t, [][]byte{data}, []string{"image"}), true)
		require.NoError(t, err)
	}
}
