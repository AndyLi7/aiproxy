package openai

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

func TestConvertVideosRequestDoesNotForwardCapability(t *testing.T) {
	t.Parallel()

	requestMeta := meta.NewMeta(nil, mode.Videos, "public-model", model.ModelConfig{})
	requestMeta.ActualModel = "upstream-model"
	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/v1/videos",
		strings.NewReader(`{"model":"public-model","capability":"text-to-video","prompt":"clouds"}`),
	)
	req.Header.Set("Content-Type", "application/json")

	converted, err := ConvertVideosRequest(requestMeta, req)
	require.NoError(t, err)
	body, err := io.ReadAll(converted.Body)
	require.NoError(t, err)
	require.JSONEq(t, `{"model":"upstream-model","prompt":"clouds"}`, string(body))
}

func TestConvertMultipartVideosRequestDoesNotForwardCapability(t *testing.T) {
	t.Parallel()

	requestMeta := meta.NewMeta(nil, mode.Videos, "public-model", model.ModelConfig{})
	requestMeta.ActualModel = "upstream-model"
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "public-model"))
	require.NoError(t, writer.WriteField("capability", "image-to-video"))
	require.NoError(t, writer.WriteField("prompt", "clouds"))
	require.NoError(t, writer.Close())
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/videos", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	converted, err := ConvertVideosRequest(requestMeta, req)
	require.NoError(t, err)
	convertedBody, err := io.ReadAll(converted.Body)
	require.NoError(t, err)
	convertedReq := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(convertedBody))
	convertedReq.Header.Set("Content-Type", converted.Header.Get("Content-Type"))
	require.NoError(t, convertedReq.ParseMultipartForm(1<<20))
	require.Equal(t, "upstream-model", convertedReq.FormValue("model"))
	require.Equal(t, "clouds", convertedReq.FormValue("prompt"))
	require.Empty(t, convertedReq.FormValue("capability"))
}
