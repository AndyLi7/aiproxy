package openai_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/adaptor/openai"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

// All markers are synthetic; no credentials or provider services are used.
const privateFailure = `FakeVendor http://private.invalid:8080 https://signed.invalid/file?signature=FAKE sk-test-only-NOT-A-KEY panic: fake stack.go:42`

func TestPublicResponseFailureHTTP200(t *testing.T) {
	for _, action := range []string{"create", "get", "cancel"} {
		t.Run(action, func(t *testing.T) {
			raw := `{"id":"resp_failed","object":"response","status":"failed","error":{"code":"private_code","message":"` + privateFailure + `"},"metadata":{"debug":"` + privateFailure + `"},"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`
			resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw))}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", nil)
			var logOutput bytes.Buffer
			logger := logrus.New()
			logger.SetOutput(&logOutput)
			common.SetLogger(c.Request, logrus.NewEntry(logger))
			middleware.RequestIDMiddleware(c)
			var result adaptor.DoResponseResult
			var err adaptor.Error
			switch action {
			case "create":
				result, err = openai.ResponseHandler(&meta.Meta{}, nil, c, resp)
			case "get":
				result, err = openai.GetResponseHandler(&meta.Meta{}, c, resp)
			case "cancel":
				result, err = openai.CancelResponseHandler(&meta.Meta{}, c, resp)
			}
			require.Nil(t, err)
			require.Equal(t, 200, w.Code) // Failed object, not an HTTP transport failure.
			require.NotEmpty(t, w.Header().Get(middleware.RequestIDHeader))
			for _, marker := range []string{"FakeVendor", "private.invalid", "signed.invalid", "signature", "sk-test", "stack.go", "panic"} {
				require.NotContains(t, w.Body.String(), marker)
			}
			require.NotContains(t, w.Body.String(), "private_code")
			require.Contains(t, w.Body.String(), `"status":"failed"`)
			require.Contains(t, w.Body.String(), "upstream_unavailable")
			require.Equal(t, "resp_failed", result.UpstreamID)
			if action != "get" {
				require.Equal(t, model.ZeroNullInt64(5), result.Usage.TotalTokens)
			}
			require.False(t, result.AsyncUsage)
			require.Contains(t, logOutput.String(), "private_code")
			require.Contains(t, logOutput.String(), middleware.GetRequestID(c))
			require.NotContains(t, logOutput.String(), "signature")
			require.NotContains(t, logOutput.String(), "sk-test")
		})
	}
}

func TestPublicResponseStreamFailureAfterContent(t *testing.T) {
	raw := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		`data: {"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"message":"` + privateFailure + `"},"metadata":{"debug":"` + privateFailure + `"}}}` + "\n\n"
	resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw))}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", nil)
	middleware.RequestIDMiddleware(c)
	result, err := openai.ResponseStreamHandler(&meta.Meta{}, nil, c, resp)
	require.Nil(t, err)
	require.Equal(t, 200, w.Code)
	require.NotEmpty(t, w.Header().Get(middleware.RequestIDHeader))
	require.Contains(t, w.Body.String(), "hello")
	require.Contains(t, w.Body.String(), "upstream_unavailable")
	for _, marker := range []string{"FakeVendor", "private.invalid", "signed.invalid", "signature", "sk-test", "stack.go", "panic"} {
		require.NotContains(t, w.Body.String(), marker)
	}
	require.Equal(t, "resp_failed", result.UpstreamID)
}
