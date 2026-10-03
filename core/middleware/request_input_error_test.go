//nolint:testpackage
package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

type publicErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param"`
	} `json:"error"`
}

// Malformed client bodies were reported as 500 gateway failures, which told
// generated clients to retry instead of fixing the request.
func TestMalformedRequestBodyIsClientError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, body := range []string{`{"model":"image","prompt":"hi"`, `not json`, `[]`, ``} {
		t.Run(body, func(t *testing.T) {
			logs := captureOperationalLogs(t)
			router := gin.New()
			router.Use(OperationalLogMiddleware())
			router.POST("/v1/images/generations", func(c *gin.Context) {
				c.Set(Group, model.GroupCache{ID: "g"})
				c.Set(Token, model.TokenCache{ID: 1})
				distribute(c, mode.ImagesGenerations)
			})

			w := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, request)

			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			var response publicErrorBody
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			require.Equal(t, "invalid_request", response.Error.Code)
			require.Equal(t, "invalid_request_error", response.Error.Type)
			require.Equal(t, "The request body must be a valid JSON object.", response.Error.Message)

			require.Len(t, *logs, 1)
			require.Equal(t, http.StatusBadRequest, (*logs)[0].Code)
			require.Equal(t, model.FailureStageValidation, (*logs)[0].FailureStage)
			require.Equal(t, "invalid_request", (*logs)[0].ErrorCode)
		})
	}
}

func TestWronglyTypedRequestFieldsNameTheParameter(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name    string
		body    string
		mode    mode.Mode
		read    func(*gin.Context, mode.Mode) error
		param   string
		message string
	}{
		{"model", `{"model":{"id":"x"}}`, mode.ChatCompletions, func(c *gin.Context, m mode.Mode) error {
			_, err := getRequestModel(c, m, "g", 1)
			return err
		}, "model", "model must be a string."},
		{"user", `{"model":"m","user":["a"]}`, mode.ChatCompletions, func(c *gin.Context, m mode.Mode) error {
			_, err := getRequestUser(c, m)
			return err
		}, "user", "user must be a string."},
		{"metadata", `{"model":"m","metadata":{"team":1}}`, mode.ImagesGenerations, func(c *gin.Context, m mode.Mode) error {
			_, err := getRequestMetadata(c, m)
			return err
		}, "metadata", "metadata must be an object whose values are strings."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")

			err := tc.read(c, tc.mode)
			require.Error(t, err)
			abortRequestFieldError(c, tc.mode, err)

			require.Equal(t, http.StatusBadRequest, w.Code)
			var response publicErrorBody
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			require.Equal(t, "invalid_parameter", response.Error.Code)
			require.Equal(t, tc.param, response.Error.Param)
			require.Equal(t, tc.message, response.Error.Message)

			fields := OperationalFieldsFromContext(c)
			require.Equal(t, model.FailureStageValidation, fields.FailureStage)
			require.Equal(t, "invalid_parameter", fields.ErrorCode)
		})
	}
}

func TestRequestFieldErrorKeepsAnthropicEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","metadata":{"user_id":{"id":1}}}`))
	c.Set(Mode, mode.Anthropic)

	_, err := getRequestUser(c, mode.Anthropic)
	require.Error(t, err)
	abortRequestFieldError(c, mode.Anthropic, err)

	require.Equal(t, http.StatusBadRequest, w.Code)
	var response struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Equal(t, "invalid_request_error", response.Error.Type)
	require.Equal(t, "metadata.user_id must be a string.", response.Error.Message)
	require.Equal(t, "invalid_parameter", OperationalFieldsFromContext(c).ErrorCode)
}

// A body that cannot be read is not the client's JSON mistake and stays a 500.
func TestRequestBodyReadFailureStaysServerError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", failingReader{})
	c.Request.ContentLength = -1

	_, err := getRequestUser(c, mode.ChatCompletions)
	require.Error(t, err)
	abortRequestFieldError(c, mode.ChatCompletions, err)
	require.Equal(t, http.StatusInternalServerError, w.Code)
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, http.ErrHandlerTimeout }
