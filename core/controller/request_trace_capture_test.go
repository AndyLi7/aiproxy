package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/labring/aiproxy/core/model"
	relaycontroller "github.com/labring/aiproxy/core/relay/controller"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
	"github.com/stretchr/testify/require"
)

func TestRelayHelperTracesEachActualAttemptWithoutOverwritingPreviousAttempt(t *testing.T) {
	c, spans, finish := newCaptureTestContext(t, context.Background())
	first := captureTestMeta(11, "upstream-a")
	second := captureTestMeta(22, "upstream-b")
	failure := relaymodel.WrapperOpenAIErrorWithMessage("secret upstream failure", "upstream_failed", http.StatusBadGateway)

	result, _ := RelayHelper(c, first, func(*gin.Context, *meta.Meta) *relaycontroller.HandleResult {
		return &relaycontroller.HandleResult{Error: failure}
	})
	require.Equal(t, failure, result.Error)
	result, _ = RelayHelper(c, second, func(*gin.Context, *meta.Meta) *relaycontroller.HandleResult {
		return &relaycontroller.HandleResult{}
	})
	require.NoError(t, result.Error)
	finish(requesttrace.StatusSuccess)

	attempts := terminalCaptureSpans(*spans, requesttrace.StageUpstreamAttempt)
	require.Len(t, attempts, 2)
	require.NotEqual(t, attempts[0].SpanID, attempts[1].SpanID)
	require.Equal(t, requesttrace.StatusError, attempts[0].Status)
	require.Equal(t, requesttrace.StatusSuccess, attempts[1].Status)
	require.Equal(t, 1, *attempts[0].Attributes.Attempt)
	require.Equal(t, 2, *attempts[1].Attributes.Attempt)
	require.Equal(t, 11, *attempts[0].Attributes.ChannelID)
	require.Equal(t, 22, *attempts[1].Attributes.ChannelID)
	require.Empty(t, attempts[0].Attributes.PublicModelID)
	require.Equal(t, "upstream-a", attempts[0].Attributes.UpstreamModelID)
	require.NotNil(t, attempts[0].DurationMS)
	require.NotNil(t, attempts[1].DurationMS)
	wire, err := json.Marshal(attempts)
	require.NoError(t, err)
	require.NotContains(t, string(wire), "secret upstream failure")
}

func TestRelayHelperClassifiesContextAndPanicOutcomes(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(context.DeadlineExceeded)
		c, spans, finish := newCaptureTestContext(t, ctx)
		RelayHelper(c, captureTestMeta(1, "mapped"), func(*gin.Context, *meta.Meta) *relaycontroller.HandleResult {
			return &relaycontroller.HandleResult{}
		})
		finish(requesttrace.StatusTimeout)
		require.Equal(t, requesttrace.StatusTimeout, terminalCaptureSpans(*spans, requesttrace.StageUpstreamAttempt)[0].Status)
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c, spans, finish := newCaptureTestContext(t, ctx)
		RelayHelper(c, captureTestMeta(1, "mapped"), func(*gin.Context, *meta.Meta) *relaycontroller.HandleResult {
			return &relaycontroller.HandleResult{}
		})
		finish(requesttrace.StatusCancelled)
		require.Equal(t, requesttrace.StatusCancelled, terminalCaptureSpans(*spans, requesttrace.StageUpstreamAttempt)[0].Status)
	})

	t.Run("panic", func(t *testing.T) {
		c, spans, finish := newCaptureTestContext(t, context.Background())
		require.PanicsWithValue(t, "boom", func() {
			RelayHelper(c, captureTestMeta(1, "mapped"), func(*gin.Context, *meta.Meta) *relaycontroller.HandleResult {
				panic("boom")
			})
		})
		finish(requesttrace.StatusError)
		require.Equal(t, requesttrace.StatusError, terminalCaptureSpans(*spans, requesttrace.StageUpstreamAttempt)[0].Status)
	})
}

func TestValidateRelayRequestTracesOnlyAnActualValidatorCall(t *testing.T) {
	c, spans, finish := newCaptureTestContext(t, context.Background())
	mc := model.NewDefaultModelConfig("public-model")
	require.NoError(t, validateRelayRequest(c, mc, nil))
	require.Empty(t, terminalCaptureSpans(*spans, requesttrace.StageValidation))

	want := errors.New("private validation detail")
	require.ErrorIs(t, validateRelayRequest(c, mc, func(*gin.Context, model.ModelConfig) error { return want }), want)
	finish(requesttrace.StatusError)
	validation := terminalCaptureSpans(*spans, requesttrace.StageValidation)
	require.Len(t, validation, 1)
	require.Equal(t, requesttrace.StatusError, validation[0].Status)
	require.Empty(t, validation[0].Attributes.ErrorCode)
}

func TestAttemptAttributesOmitUnverifiedRequestModels(t *testing.T) {
	m := captureTestMeta(9, "incoming-model")
	m.Channel.ModelMapping = nil
	m.ActualModel = "incoming-model"
	attrs := requestTraceAttemptAttributes(m, 1)
	require.Empty(t, attrs.PublicModelID)
	require.Empty(t, attrs.UpstreamModelID)
}

func TestAttemptAttributesOmitSyntheticDefaultConfigModel(t *testing.T) {
	const attackerModel = "attacker-supplied-model"
	m := meta.NewMeta(nil, mode.ImagesGenerations, attackerModel, model.NewDefaultModelConfig(attackerModel))
	attrs := requestTraceAttemptAttributes(m, 1)
	require.Empty(t, attrs.PublicModelID)
	require.Empty(t, attrs.UpstreamModelID)
	wire, err := json.Marshal(attrs)
	require.NoError(t, err)
	require.NotContains(t, string(wire), attackerModel)
}

func TestAttemptAttributesUseValidatedCapabilityPublicIdentity(t *testing.T) {
	t.Run("video capability uses public parent not internal route", func(t *testing.T) {
		mc := model.NewDefaultModelConfig("video-model::text-to-video")
		mc.Config = map[model.ModelConfigKey]any{
			"capability_contract_version": model.ModelCapabilityContractVersion,
			"public_model":                "video-model",
			"capability":                  "text-to-video",
		}
		m := meta.NewMeta(nil, mode.Videos, "video-model", mc, meta.WithRoutingModel(mc.Model))
		attrs := requestTraceAttemptAttributes(m, 1)
		require.Equal(t, "video-model", attrs.PublicModelID)
		require.NotEqual(t, mc.Model, attrs.PublicModelID)
	})

	t.Run("image capability uses validated public capability ID", func(t *testing.T) {
		mc := model.NewDefaultModelConfig("image-model::text-to-image")
		mc.Config = map[model.ModelConfigKey]any{
			"capability_contract_version": model.ModelCapabilityContractVersion,
			"public_model":                "image-model",
			"capability":                  "text-to-image",
			"public_capability_model":     "image-model/text-to-image",
		}
		m := meta.NewMeta(nil, mode.ImagesGenerations, "image-model/text-to-image", mc, meta.WithRoutingModel(mc.Model))
		attrs := requestTraceAttemptAttributes(m, 1)
		require.Equal(t, "image-model/text-to-image", attrs.PublicModelID)
		require.NotEqual(t, mc.Model, attrs.PublicModelID)
	})
}

func captureTestMeta(channelID int, mapped string) *meta.Meta {
	m := meta.NewMeta(nil, mode.ImagesGenerations, "attacker-input", model.NewDefaultModelConfig("public-model"), meta.WithRoutingModel("public-model"))
	m.SetChannel(&model.Channel{ID: channelID, ModelMapping: map[string]string{"public-model": mapped}})
	return m
}

func newCaptureTestContext(t *testing.T, ctx context.Context) (*gin.Context, *[]requesttrace.Span, func(requesttrace.Status)) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	spans := make([]requesttrace.Span, 0, 8)
	session := requesttrace.NewSession("capture-request", func(span requesttrace.Span) bool {
		spans = append(spans, span)
		return true
	})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/images/generations", nil)
	c.Set("request_trace_session", session)
	return c, &spans, func(status requesttrace.Status) { require.True(t, session.Finish(status)) }
}

func terminalCaptureSpans(spans []requesttrace.Span, stage requesttrace.Stage) []requesttrace.Span {
	result := make([]requesttrace.Span, 0)
	for _, span := range spans {
		if span.Stage == stage && span.Status != requesttrace.StatusRunning {
			result = append(result, span)
		}
	}
	return result
}
