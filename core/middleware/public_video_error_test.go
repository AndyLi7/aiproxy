//nolint:testpackage
package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/relay/mode"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
	"github.com/stretchr/testify/require"
)

func TestIsPublicVideoRequestRecognizesPathsAndVideoModes(t *testing.T) {
	t.Parallel()

	require.True(t, IsPublicVideoRequest("/v1/videos", mode.Unknown))
	require.True(t, IsPublicVideoRequest("/v1/videos/video-id", mode.Unknown))
	require.True(t, IsPublicVideoRequest("/other", mode.Videos))
	require.True(t, IsPublicVideoRequest("/other", mode.DoubaoVideo))
	require.False(t, IsPublicVideoRequest("/v1/models", mode.Unknown))
	require.False(t, IsPublicVideoRequest("/v1/chat/completions", mode.ChatCompletions))
}

func TestAbortWithMessageSanitizesVideoAuthenticationBeforeModeSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)

	AbortWithMessage(c, http.StatusUnauthorized, "database lookup exposed private-key")

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	var body relaymodel.OpenAIErrorResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, "The API key is missing or invalid.", body.Error.Message)
	require.Equal(t, "authentication_error", body.Error.Type)
	require.Equal(t, "invalid_api_key", body.Error.Code)
	require.NotContains(t, recorder.Body.String(), "private-key")
}

func TestAbortPublicVideoRequestErrorIncludesActionableFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)

	AbortPublicVideoRequestError(
		c,
		"validation",
		http.StatusBadRequest,
		"unsupported_by_model",
		"seconds 3 is not supported by this model",
		"seconds",
		"3",
		[]string{"4", "5"},
		"one of the allowed integer durations",
	)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	var body relaymodel.OpenAIErrorResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, "unsupported_by_model", body.Error.Code)
	require.Equal(t, "seconds", body.Error.Param)
	require.Equal(t, "3", body.Error.Value)
	require.Equal(t, []string{"4", "5"}, *body.Error.AllowedValues)
	require.Equal(t, "one of the allowed integer durations", body.Error.Expected)
}
