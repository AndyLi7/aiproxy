//nolint:testpackage // Validate the final public boundary used by relay and retry.
package controller

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/adaptor/openai"
	relaycontroller "github.com/labring/aiproxy/core/relay/controller"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
	"github.com/stretchr/testify/require"
)

const syntheticUpstreamFailure = `FakeVendor http://private.invalid:8080 https://signed.invalid/file?signature=FAKE sk-test-only-NOT-A-KEY panic: fake stack.go:42`

func TestRelayPublicErrorProtocolMatrix(t *testing.T) {
	for _, protocol := range []struct {
		name, path string
		mode       mode.Mode
	}{
		{"openai", "/v1/chat/completions", mode.ChatCompletions},
		{"anthropic", "/v1/messages", mode.Anthropic},
		{"gemini", "/v1beta/models/test:generateContent", mode.Gemini},
		{"video", "/v1/videos", mode.Videos},
	} {
		for _, status := range []int{400, 422, 401, 402, 403, 404, 429, 500, 503} {
			for _, id := range []string{"request-safe-123", "", "unsafe\nrequest"} {
				t.Run(
					protocol.name+"/"+strconv.Itoa(status)+"/"+strconv.Quote(id),
					func(t *testing.T) {
						w := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(w)
						c.Request = httptest.NewRequestWithContext(
							t.Context(),
							http.MethodPost,
							protocol.path,
							nil,
						)
						c.Request.Header.Set(middleware.RequestIDHeader, id)
						c.Set(middleware.Mode, protocol.mode)
						middleware.RequestIDMiddleware(c)
						requestID := middleware.GetRequestID(c)
						require.NotEmpty(t, requestID)

						if id == "request-safe-123" {
							require.Equal(t, id, requestID)
						}

						raw := `{"error":{"message":"` + syntheticUpstreamFailure + `","type":"private_type","code":"private_code","param":"private_param"},"debug":"private_debug"}`
						original := openai.ErrorHanlder(
							&http.Response{
								StatusCode: status,
								Header:     make(http.Header),
								Body:       io.NopCloser(strings.NewReader(raw)),
							},
						)
						diagnostic := original.Error()
						require.True(t, handleRelayResult(c, original, false, 0, time.Time{}))

						wantStatus := status
						if status >= 401 && status <= 404 {
							wantStatus = 502
						}

						require.Equal(t, wantStatus, w.Code)
						require.Equal(t, requestID, w.Header().Get(middleware.RequestIDHeader))

						for _, marker := range []string{"FakeVendor", "private", "signed.invalid", "signature", "sk-test", "stack.go", "panic"} {
							require.NotContains(t, w.Body.String(), marker)
						}

						require.Equal(t, diagnostic, original.Error())
						require.Contains(t, original.Error(), syntheticUpstreamFailure)

						if status == 400 || status == 422 {
							require.Contains(t, w.Body.String(), "input parameters")
						}
					},
				)
			}
		}
	}
}

func TestRelayAdapterFailureWithoutRequestID(t *testing.T) {
	// Defensive direct invocation without the router middleware must remain safe.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/chat/completions",
		nil,
	)
	original := relaymodel.WrapperErrorWithMessage(
		mode.ChatCompletions,
		500,
		syntheticUpstreamFailure,
	)
	require.True(t, handleRelayResult(c, original, true, 0, time.Time{}))
	require.Equal(t, 500, w.Code)
	require.NotContains(t, w.Body.String(), "FakeVendor")
	require.Contains(t, w.Body.String(), "upstream_unavailable")
	require.Contains(t, original.Error(), syntheticUpstreamFailure)
}

// Embed unused methods; any accidental extra adapter call fails the test rather
// than reaching a real provider. ResponsesGet skips write admission/database use.
type failingBoundaryAdapter struct {
	adaptor.Adaptor
	stage string
}

func (a failingBoundaryAdapter) ConvertRequest(
	_ *meta.Meta,
	_ adaptor.Store,
	_ *http.Request,
) (adaptor.ConvertResult, error) {
	if a.stage == "convert" {
		return adaptor.ConvertResult{}, errors.New(syntheticUpstreamFailure)
	}
	return adaptor.ConvertResult{Body: strings.NewReader("{}")}, nil
}

func (a failingBoundaryAdapter) GetRequestURL(
	_ *meta.Meta,
	_ adaptor.Store,
	_ *gin.Context,
) (adaptor.RequestURL, error) {
	if a.stage == "url" {
		return adaptor.RequestURL{}, errors.New(syntheticUpstreamFailure)
	}
	return adaptor.RequestURL{URL: "http://provider.invalid/response", Method: http.MethodGet}, nil
}

func (a failingBoundaryAdapter) SetupRequestHeader(
	_ *meta.Meta,
	_ adaptor.Store,
	_ *gin.Context,
	_ *http.Request,
) error {
	if a.stage == "header" {
		return errors.New(syntheticUpstreamFailure)
	}
	return nil
}

func (a failingBoundaryAdapter) DoRequest(
	_ *meta.Meta,
	_ adaptor.Store,
	_ *gin.Context,
	_ *http.Request,
) (*http.Response, error) {
	if a.stage == "transport" {
		return nil, errors.New(syntheticUpstreamFailure)
	}

	return &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(syntheticUpstreamFailure)),
	}, nil
}

func (a failingBoundaryAdapter) DoResponse(
	_ *meta.Meta,
	_ adaptor.Store,
	_ *gin.Context,
	resp *http.Response,
) (adaptor.DoResponseResult, adaptor.Error) {
	return adaptor.DoResponseResult{}, openai.ErrorHanlder(resp)
}

func TestRelayAdapterStagesPublicBoundary(t *testing.T) {
	for _, stage := range []string{"convert", "url", "header", "transport", "response"} {
		t.Run(stage, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequestWithContext(
				t.Context(),
				http.MethodGet,
				"/v1/responses/resp_fake",
				nil,
			)
			middleware.RequestIDMiddleware(c)

			m := &meta.Meta{
				Mode:    mode.ResponsesGet,
				Channel: meta.ChannelMeta{BaseURL: "http://provider.invalid"},
			}
			_, detail, err := relaycontroller.DoHelper(
				failingBoundaryAdapter{stage: stage},
				c,
				m,
				nil,
				relaycontroller.BodyDetailOption{IncludeResponseBody: true},
			)
			require.NotNil(t, err)
			require.Contains(t, err.Error(), syntheticUpstreamFailure)
			require.False(t, c.Writer.Written())
			require.True(t, handleRelayResult(c, err, false, 0, time.Time{}))
			require.Equal(t, err.StatusCode(), w.Code)
			require.NotContains(t, w.Body.String(), "FakeVendor")
			require.NotContains(t, w.Body.String(), "signature")
			require.NotContains(t, w.Body.String(), "stack.go")
			require.NotEmpty(t, w.Header().Get(middleware.RequestIDHeader))

			if stage == "response" {
				require.Contains(t, detail.ResponseBody, "FakeVendor")
			}
		})
	}
}
