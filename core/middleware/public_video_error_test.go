//nolint:testpackage
package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
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

func TestAbortUnsupportedPublicVideoModelListsOnlyAccessibleVideoModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)

	token := model.TokenCache{}
	token.SetAvailableSets([]string{"default"})
	token.SetModelsBySet(map[string][]string{
		"default": {
			"bytedance/seedance-1.0-pro",
			"openai/gpt-5",
		},
	})
	caches := &model.ModelCaches{
		EnabledModelConfigsMap: map[string]model.ModelConfig{
			"bytedance/seedance-1.0-pro": {
				Model: "bytedance/seedance-1.0-pro",
				Type:  mode.DoubaoVideo,
			},
			"openai/gpt-5": {
				Model: "openai/gpt-5",
				Type:  mode.ChatCompletions,
			},
		},
	}

	abortUnsupportedPublicVideoModel(
		c,
		mode.Videos,
		"bytedance/does-not-exist",
		token,
		caches,
	)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	var body relaymodel.OpenAIErrorResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, "unsupported_by_model", body.Error.Code)
	require.Equal(t, "model", body.Error.Param)
	require.Equal(t, "bytedance/does-not-exist", body.Error.Value)
	require.Equal(
		t,
		[]string{"bytedance/seedance-1.0-pro"},
		*body.Error.AllowedValues,
	)
	require.Equal(t, "one of the allowed video model IDs", body.Error.Expected)
}
