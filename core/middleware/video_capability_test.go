//nolint:testpackage
package middleware

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coremodel "github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

func TestResolveInitialVideoCapabilityJSON(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(`{"model":"bytedance/seedance-2.0","capability":"image-to-video"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = request

	publicModel, routingModel, err := resolveVideoCapability(
		ctx,
		mode.Videos,
		"bytedance/seedance-2.0",
	)
	require.NoError(t, err)
	require.Equal(t, "bytedance/seedance-2.0", publicModel)
	require.Equal(t, "bytedance/seedance-2.0::image-to-video", routingModel)
	require.Equal(t, "bytedance/seedance-2.0", GetPublicRequestModel(ctx))
	require.Equal(t, "image-to-video", GetVideoCapability(ctx))
}

func TestResolveInitialVideoCapabilityMultipart(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "bytedance/seedance-2.0"))
	require.NoError(t, writer.WriteField("capability", "text-to-video"))
	require.NoError(t, writer.Close())

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/videos", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = request

	publicModel, routingModel, err := resolveVideoCapability(
		ctx,
		mode.Videos,
		"bytedance/seedance-2.0",
	)
	require.NoError(t, err)
	require.Equal(t, "bytedance/seedance-2.0", publicModel)
	require.Equal(t, "bytedance/seedance-2.0::text-to-video", routingModel)
}

func TestResolveInitialVideoCapabilityReturnsPublicErrors(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name string
		body string
		code string
	}{
		{name: "missing", body: `{"model":"bytedance/seedance-2.0"}`, code: "missing_capability"},
		{name: "unsupported", body: `{"model":"bytedance/seedance-2.0","capability":"video-to-video"}`, code: "unsupported_capability"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequestWithContext(
				t.Context(),
				http.MethodPost,
				"/v1/videos",
				bytes.NewBufferString(tc.body),
			)
			request.Header.Set("Content-Type", "application/json")
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = request

			_, _, err := resolveVideoCapability(ctx, mode.Videos, "bytedance/seedance-2.0")
			validationErr, ok := err.(*publicVideoRequestValidationError)
			require.True(t, ok)
			require.Equal(t, tc.code, validationErr.code)
			require.Equal(t, "capability", validationErr.param)
		})
	}
}

func TestResolveInitialVideoCapabilityRejectsInternalRouteKey(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	request := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/v1/videos",
		bytes.NewBufferString(`{"model":"bytedance/seedance-2.0::text-to-video"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = request

	_, _, err := resolveVideoCapability(
		ctx, mode.Videos, "bytedance/seedance-2.0::text-to-video",
	)
	validationErr, ok := err.(*publicVideoRequestValidationError)
	require.True(t, ok)
	require.Equal(t, "invalid_parameter", validationErr.code)
	require.Equal(t, "model", validationErr.param)
	require.NotContains(t, validationErr.message, "::")
}

func TestResolveStoredVideoCapabilityDoesNotRequireRequestField(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/videos/video-1", nil)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = request

	publicModel, routingModel, err := resolveVideoCapability(
		ctx,
		mode.VideosGet,
		"bytedance/seedance-2.0::text-to-video",
	)
	require.NoError(t, err)
	require.Equal(t, "bytedance/seedance-2.0", publicModel)
	require.Equal(t, "bytedance/seedance-2.0::text-to-video", routingModel)
	require.Equal(t, string(coremodel.ModelCapabilityTextToVideo), GetVideoCapability(ctx))
}

func TestPublicVideoModelIDHidesCapabilityRouteKey(t *testing.T) {
	t.Parallel()

	require.Equal(
		t,
		"bytedance/seedance-2.0",
		publicVideoModelID("bytedance/seedance-2.0::image-to-video"),
	)
	require.Equal(t, "legacy-model", publicVideoModelID("legacy-model"))
}
