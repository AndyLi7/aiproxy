//nolint:testpackage
package controller

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestGetVideosRequestUsageMultipart(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	var body bytes.Buffer

	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "sora-2"))
	require.NoError(t, writer.WriteField("prompt", "Animate the reference"))
	require.NoError(t, writer.WriteField("size", "1280x720"))
	require.NoError(t, writer.WriteField("seconds", "6"))
	require.NoError(t, writer.Close())

	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		&body,
	)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	usage, err := GetVideosRequestUsage(ctx, model.ModelConfig{})
	require.NoError(t, err)
	require.Zero(t, usage.Usage.OutputTokens)
	require.Zero(t, usage.Usage.TotalTokens)
}

func TestValidateVideosRequestEnforcesCapabilityInputShape(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name       string
		capability string
		body       string
		param      string
	}{
		{
			name:       "image capability requires image",
			capability: "image-to-video",
			body:       `{"prompt":"A city street","size":"1280x720"}`,
			param:      "input_reference",
		},
		{
			name:       "text capability rejects image",
			capability: "text-to-video",
			body:       `{"prompt":"A city street","size":"1280x720","input_reference":"https://example.com/frame.png"}`,
			param:      "input_reference",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(
				t.Context(), http.MethodPost, "/v1/videos", bytes.NewBufferString(tc.body),
			)
			req.Header.Set("Content-Type", "application/json")
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = req

			err := ValidateVideosRequest(ctx, model.ModelConfig{Config: map[model.ModelConfigKey]any{
				"capability": tc.capability,
			}})
			var paramErr *RequestParamError
			require.ErrorAs(t, err, &paramErr)
			require.Equal(t, tc.param, paramErr.Param)
		})
	}
}

func TestValidateVideosRequestAcceptsCapabilityInputShape(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/v1/videos", bytes.NewBufferString(
			`{"prompt":"A city street","size":"1280x720","input_reference":"https://example.com/frame.png"}`,
		),
	)
	req.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{Config: map[model.ModelConfigKey]any{
		"capability": "image-to-video",
	}})
	require.NoError(t, err)
}

func TestValidateVideosRequestRejectsTooLongSeconds(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	body := `{
		"model":"video-model",
		"prompt":"A city street",
		"size":"1280x720",
		"seconds":6
	}`
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(body),
	)
	req.Header.Set("Content-Type", "application/json")

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{
		MaxVideoGenerationSeconds: 5,
	})
	require.Error(t, err)
	require.Equal(t, "seconds must be less than or equal to 5", err.Error())

	var requestParamErr *RequestParamError
	require.ErrorAs(t, err, &requestParamErr)
	require.Equal(t, 400, requestParamErr.StatusCode)
}

func TestValidateVideosRequestRejectsUnsupportedMultipartSize(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	var body bytes.Buffer

	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "video-model"))
	require.NoError(t, writer.WriteField("prompt", "A city street"))
	require.NoError(t, writer.WriteField("size", "1920x1080"))
	require.NoError(t, writer.WriteField("seconds", "5"))
	require.NoError(t, writer.Close())

	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos/video-123/remix",
		&body,
	)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{
		AllowedResolutions: []string{"720p"},
	})
	require.Error(t, err)
	require.Equal(
		t,
		"unsupported video resolution `1920x1080`, supported resolutions: 1280x720",
		err.Error(),
	)
}

func TestValidateVideosRequestRejectsInvalidSizeFormat(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	body := `{
		"model":"video-model",
		"prompt":"A city street",
		"size":"720P",
		"seconds":5
	}`
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(body),
	)
	req.Header.Set("Content-Type", "application/json")

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{
		AllowedResolutions: []string{"720p"},
	})
	require.Error(t, err)
	require.Equal(t, "invalid video size `720p`, allowed values: 1280x720", err.Error())
}

func TestGetVideosRequestUsageRejectsNonOpenAIDimensionDelimiter(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	body := `{
		"model":"video-model",
		"prompt":"A city street",
		"size":"1280*720",
		"seconds":5
	}`
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(body),
	)
	req.Header.Set("Content-Type", "application/json")

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	_, err := GetVideosRequestUsage(ctx, model.ModelConfig{})
	require.Error(t, err)
	require.Equal(
		t,
		"invalid video size `1280*720`: expected <width>x<height>",
		err.Error(),
	)
}

func TestValidateVideosRequestAllowsAdvertised4KSize(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	body := `{
		"model":"video-model",
		"prompt":"A city street",
		"size":"3840x2160",
		"seconds":5
	}`
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(body),
	)
	req.Header.Set("Content-Type", "application/json")

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{
		AllowedResolutions: []string{"4k"},
	})
	require.NoError(t, err)
}

func TestValidateVideosRequestRejectsFuzzySizeWhenCapabilitiesArePublished(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(`{
			"model":"video-model",
			"prompt":"A city street",
			"size":"999x999",
			"seconds":5
		}`),
	)
	req.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{
		AllowedResolutions: []string{"480p", "720p"},
		Config: map[model.ModelConfigKey]any{
			"resolutions":  []any{"480p", "720p"},
			"aspectRatios": []any{"16:9", "9:16", "1:1"},
			"durations":    []any{float64(5), float64(10)},
		},
	})
	require.EqualError(
		t,
		err,
		"unsupported video size `999x999`, allowed values: 854x480, 480x854, 480x480, 1280x720, 720x1280, 720x720",
	)
	var paramErr *RequestParamError
	require.ErrorAs(t, err, &paramErr)
	require.Equal(t, "unsupported_by_model", paramErr.Code)
	require.Equal(t, "size", paramErr.Param)
	require.Equal(t, "999x999", paramErr.Value)
	require.Equal(t, []string{"854x480", "480x854", "480x480", "1280x720", "720x1280", "720x720"}, paramErr.AllowedValues)
}

func TestValidateVideosRequestRejectsMalformedSizeWithActionableDetails(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(`{
			"model":"video-model",
			"prompt":"A city street",
			"size":"1280*720",
			"seconds":5
		}`),
	)
	req.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{
		Config: map[model.ModelConfigKey]any{
			"resolutions":  []any{"720p"},
			"aspectRatios": []any{"16:9", "9:16"},
		},
	})
	var paramErr *RequestParamError
	require.ErrorAs(t, err, &paramErr)
	require.Equal(t, "invalid_parameter", paramErr.Code)
	require.Equal(t, "size", paramErr.Param)
	require.Equal(t, "1280*720", paramErr.Value)
	require.Equal(t, []string{"1280x720", "720x1280"}, paramErr.AllowedValues)
	require.Equal(t, "<width>x<height> string", paramErr.Expected)
}

func TestValidateVideosRequestAllowsExactCapabilitySize(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(`{
			"model":"video-model",
			"prompt":"A city street",
			"size":"720x720",
			"seconds":10
		}`),
	)
	req.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{
		Config: map[model.ModelConfigKey]any{
			"resolutions":  []any{"720p"},
			"aspectRatios": []any{"1:1"},
			"durations":    []any{float64(5), float64(10)},
		},
	})
	require.NoError(t, err)
}

func TestValidateVideosRequestAllowsSemanticDimensions(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(`{
			"model":"video-model",
			"prompt":"A city street",
			"resolution":"720p",
			"aspect_ratio":"16:9",
			"seconds":5
		}`),
	)
	req.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{
		Config: map[model.ModelConfigKey]any{
			"resolutions":  []any{"480p", "720p"},
			"aspectRatios": []any{"16:9", "9:16"},
			"durations":    []any{float64(5), float64(10)},
		},
	})
	require.NoError(t, err)
}

func TestValidateVideosRequestAllowsPublishedExtendedVideoDimensions(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	for _, selection := range []struct {
		name        string
		size        string
		aspectRatio string
	}{
		{name: "ultrawide", size: "1680x720", aspectRatio: "21:9"},
		{name: "landscape four three", size: "960x720", aspectRatio: "4:3"},
		{name: "portrait three four", size: "720x960", aspectRatio: "3:4"},
	} {
		t.Run(selection.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(
				t.Context(),
				http.MethodPost,
				"/v1/videos",
				bytes.NewBufferString(`{"model":"video-model","prompt":"A city street","size":"`+
					selection.size+`","seconds":5}`),
			)
			req.Header.Set("Content-Type", "application/json")
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = req

			err := ValidateVideosRequest(ctx, model.ModelConfig{
				Config: map[model.ModelConfigKey]any{
					"resolutions":  []any{"720p"},
					"aspectRatios": []any{"16:9", selection.aspectRatio},
					"durations":    []any{float64(5)},
				},
			})
			require.NoError(t, err)
		})
	}
}

func TestValidateVideosRequestRejectsFixedSizeForAdaptiveOnlyModel(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(`{"model":"video-model","prompt":"A city street","size":"1280x720","seconds":5}`),
	)
	req.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{
		AllowedResolutions: []string{"720p"},
		Config: map[model.ModelConfigKey]any{
			"resolutions":  []any{"720p"},
			"aspectRatios": []any{"adaptive"},
			"durations":    []any{float64(5)},
		},
	})
	var paramErr *RequestParamError
	require.ErrorAs(t, err, &paramErr)
	require.Equal(t, "unsupported_by_model", paramErr.Code)
	require.Equal(t, "size", paramErr.Param)
	require.Empty(t, paramErr.AllowedValues)
}

func TestGetVideosRequestUsagePreservesSemanticBillingContext(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(`{
			"model":"video-model",
			"prompt":"A city street",
			"resolution":"720p",
			"aspect_ratio":"16:9",
			"seconds":5,
			"generate_audio":true
		}`),
	)
	req.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	usage, err := GetVideosRequestUsage(ctx, model.ModelConfig{})
	require.NoError(t, err)
	require.Equal(t, "720p", usage.Context.NativeResolution)
	require.Empty(t, usage.Context.Resolution)
	require.Equal(t, 5, usage.Context.Seconds)
	require.NotNil(t, usage.Context.OutputAudio)
	require.True(t, *usage.Context.OutputAudio)
}

func TestValidateVideosRequestRejectsUnsupportedSemanticDimensions(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	tests := []struct {
		name        string
		resolution  string
		aspectRatio string
		wantParam   string
		wantValue   string
		wantAllowed []string
	}{
		{
			name:        "resolution",
			resolution:  "1080p",
			aspectRatio: "16:9",
			wantParam:   "resolution",
			wantValue:   "1080p",
			wantAllowed: []string{"480p", "720p"},
		},
		{
			name:        "aspect ratio",
			resolution:  "720p",
			aspectRatio: "1:1",
			wantParam:   "aspect_ratio",
			wantValue:   "1:1",
			wantAllowed: []string{"16:9", "9:16"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			body := `{"model":"video-model","prompt":"A city street","resolution":"` +
				test.resolution + `","aspect_ratio":"` + test.aspectRatio + `","seconds":5}`
			req := httptest.NewRequestWithContext(
				t.Context(), http.MethodPost, "/v1/videos", bytes.NewBufferString(body),
			)
			req.Header.Set("Content-Type", "application/json")
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = req

			err := ValidateVideosRequest(ctx, model.ModelConfig{
				Config: map[model.ModelConfigKey]any{
					"resolutions":  []any{"480p", "720p"},
					"aspectRatios": []any{"16:9", "9:16"},
				},
			})
			var paramErr *RequestParamError
			require.ErrorAs(t, err, &paramErr)
			require.Equal(t, "unsupported_by_model", paramErr.Code)
			require.Equal(t, test.wantParam, paramErr.Param)
			require.Equal(t, test.wantValue, paramErr.Value)
			require.Equal(t, test.wantAllowed, paramErr.AllowedValues)
		})
	}
}

func TestValidateVideosRequestRejectsAmbiguousDimensionSelection(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	tests := []struct {
		name      string
		body      string
		wantCode  string
		wantParam string
	}{
		{
			name: "size with semantic dimensions",
			body: `{"model":"video-model","prompt":"A city street","size":"1280x720",` +
				`"resolution":"720p","aspect_ratio":"16:9","seconds":5}`,
			wantCode:  "invalid_parameter",
			wantParam: "size",
		},
		{
			name:      "resolution without aspect ratio",
			body:      `{"model":"video-model","prompt":"A city street","resolution":"720p","seconds":5}`,
			wantCode:  "missing_parameter",
			wantParam: "aspect_ratio",
		},
		{
			name:      "aspect ratio without resolution",
			body:      `{"model":"video-model","prompt":"A city street","aspect_ratio":"16:9","seconds":5}`,
			wantCode:  "missing_parameter",
			wantParam: "resolution",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(
				t.Context(), http.MethodPost, "/v1/videos", bytes.NewBufferString(test.body),
			)
			req.Header.Set("Content-Type", "application/json")
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = req

			err := ValidateVideosRequest(ctx, model.ModelConfig{})
			var paramErr *RequestParamError
			require.ErrorAs(t, err, &paramErr)
			require.Equal(t, test.wantCode, paramErr.Code)
			require.Equal(t, test.wantParam, paramErr.Param)
		})
	}
}

func TestValidateVideosRequestRejectsUnsupportedDiscreteDuration(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(`{
			"model":"video-model",
			"prompt":"A city street",
			"size":"1280x720",
			"seconds":6
		}`),
	)
	req.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{
		Config: map[model.ModelConfigKey]any{
			"resolutions":  []any{"720p"},
			"aspectRatios": []any{"16:9"},
			"durations":    []any{float64(5), float64(10), float64(12)},
		},
	})
	require.EqualError(
		t,
		err,
		"unsupported video duration `6`, allowed values: 5, 10, 12",
	)
	var paramErr *RequestParamError
	require.ErrorAs(t, err, &paramErr)
	require.Equal(t, "unsupported_by_model", paramErr.Code)
	require.Equal(t, "seconds", paramErr.Param)
	require.Equal(t, "6", paramErr.Value)
	require.Equal(t, []string{"5", "10", "12"}, paramErr.AllowedValues)
}

func TestValidateVideosRequestRejectsExplicitNullSeconds(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(`{"model":"video-model","prompt":"A city street","seconds":null}`),
	)
	req.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{})
	var paramErr *RequestParamError
	require.ErrorAs(t, err, &paramErr)
	require.Equal(t, "invalid_parameter", paramErr.Code)
	require.Equal(t, "seconds", paramErr.Param)
	require.Equal(t, "positive integer", paramErr.Expected)
}

func TestValidateVideosRequestRejectsFractionalSeconds(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(`{
			"model":"video-model",
			"prompt":"A city street",
			"size":"1280x720",
			"seconds":2.5
		}`),
	)
	req.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{})
	var paramErr *RequestParamError
	require.ErrorAs(t, err, &paramErr)
	require.Equal(t, "invalid_parameter", paramErr.Code)
	require.Equal(t, "seconds", paramErr.Param)
	require.Equal(t, "positive integer", paramErr.Expected)
}

func TestValidateVideosRequestRequiresSize(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(`{
			"model":"video-model",
			"prompt":"A city street",
			"seconds":5
		}`),
	)
	req.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{})
	var paramErr *RequestParamError
	require.ErrorAs(t, err, &paramErr)
	require.Equal(t, "missing_parameter", paramErr.Code)
	require.Equal(t, "size", paramErr.Param)
	require.Equal(t, "<width>x<height> string", paramErr.Expected)
}

func TestValidateVideosRequestMultipartRequiresSize(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "video-model"))
	require.NoError(t, writer.WriteField("prompt", "A city street"))
	require.NoError(t, writer.WriteField("seconds", "5"))
	require.NoError(t, writer.Close())

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/videos", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{})
	var paramErr *RequestParamError
	require.ErrorAs(t, err, &paramErr)
	require.Equal(t, "missing_parameter", paramErr.Code)
	require.Equal(t, "size", paramErr.Param)
	require.Equal(t, "<width>x<height> string", paramErr.Expected)
}

func TestValidateVideosRequestRejectsNonObjectBody(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/v1/videos", bytes.NewBufferString(`[]`),
	)
	req.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{})
	var paramErr *RequestParamError
	require.ErrorAs(t, err, &paramErr)
	require.Equal(t, "body", paramErr.Param)
	require.Equal(t, "array", paramErr.Value)
	require.Equal(t, "JSON object", paramErr.Expected)
}

func TestValidateVideosRequestRequiresPrompt(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/v1/videos", bytes.NewBufferString(`{"model":"video-model"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{})
	var paramErr *RequestParamError
	require.ErrorAs(t, err, &paramErr)
	require.Equal(t, "missing_parameter", paramErr.Code)
	require.Equal(t, "prompt", paramErr.Param)
}

func TestValidateVideosRequestRejectsUnsupportedAudio(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(`{
			"model":"video-model",
			"prompt":"A city street",
			"size":"1280x720",
			"seconds":5,
			"generate_audio":true
		}`),
	)
	req.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	err := ValidateVideosRequest(ctx, model.ModelConfig{
		Config: map[model.ModelConfigKey]any{
			"resolutions":  []any{"720p"},
			"aspectRatios": []any{"16:9"},
			"durations":    []any{float64(5)},
			"audio": map[string]any{
				"mode":           "none",
				"defaultEnabled": false,
			},
		},
	})
	require.EqualError(
		t,
		err,
		"generate_audio is not supported by this model; allowed value: false",
	)
}

func TestGetVideosRequestUsageIgnoresJobOnlyFields(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	body := `{
		"model":"video-model",
		"prompt":"A city street",
		"seconds":4,
		"size":"1280x720",
		"n_seconds":60,
		"n_variants":10,
		"width":1920,
		"height":1080
	}`
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(body),
	)
	req.Header.Set("Content-Type", "application/json")

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	usage, err := GetVideosRequestUsage(ctx, model.ModelConfig{
		AllowedResolutions:        []string{"720p"},
		MaxVideoGenerationSeconds: 5,
	})
	require.NoError(t, err)
	require.Zero(t, usage.Usage.OutputTokens)
	require.Equal(t, "1280x720", usage.Context.Resolution)
}

func TestGetVideosRequestUsageUsesOfficialVideoFields(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	body := `{
		"model":"video-model",
		"prompt":"A city street",
		"seconds":4,
		"size":"1280x720",
		"n_seconds":60,
		"n_variants":10,
		"width":1920,
		"height":1080
	}`
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(body),
	)
	req.Header.Set("Content-Type", "application/json")

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	usage, err := GetVideosRequestUsage(ctx, model.ModelConfig{})
	require.NoError(t, err)
	require.Zero(t, usage.Usage.OutputTokens)
	require.Zero(t, usage.Usage.TotalTokens)
	require.Equal(t, "1280x720", usage.Context.Resolution)
}
