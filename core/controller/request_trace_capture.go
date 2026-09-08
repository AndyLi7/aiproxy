package controller

import (
	"context"
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	relaycontroller "github.com/labring/aiproxy/core/relay/controller"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
)

func validateRelayRequest(c *gin.Context, mc model.ModelConfig, validator ValidateRequest) (err error) {
	if validator == nil {
		return nil
	}
	handle := middleware.BeginRequestTraceStage(c, requesttrace.StageValidation, requesttrace.Attributes{})
	defer func() {
		if recovered := recover(); recovered != nil {
			handle.Finish(requesttrace.StatusError)
			panic(recovered)
		}
		if err != nil {
			handle.Finish(requesttrace.StatusError)
		} else {
			handle.Finish(requesttrace.StatusSuccess)
		}
	}()
	return validator(c, mc)
}

func requestTraceAttemptAttributes(m *meta.Meta, attempt int) requesttrace.Attributes {
	attrs := requesttrace.Attributes{Attempt: &attempt}
	if m == nil {
		return attrs
	}
	channelID := m.Channel.ID
	attrs.ChannelID = &channelID
	publicModel := m.ModelConfig.Model
	if publicModel == "" {
		return attrs
	}
	attrs.PublicModelID = publicModel
	if mapped, ok := meta.GetMappedModelName(publicModel, m.Channel.ModelMapping); ok && mapped == m.ActualModel {
		attrs.UpstreamModelID = mapped
	}
	return attrs
}

func requestTraceResultStatus(ctx context.Context, result *relaycontroller.HandleResult) requesttrace.Status {
	if ctx != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			return requesttrace.StatusTimeout
		}
		if ctx.Err() != nil {
			return requesttrace.StatusCancelled
		}
	}
	if result == nil || result.Error != nil {
		return requesttrace.StatusError
	}
	return requesttrace.StatusSuccess
}

func getInitialChannelWithTrace(c *gin.Context, modelName string, relayMode mode.Mode) (selected *initialChannel, err error) {
	handle := middleware.BeginRequestTraceStage(c, requesttrace.StageChannelSelection, requesttrace.Attributes{})
	defer func() {
		if recovered := recover(); recovered != nil {
			handle.Finish(requesttrace.StatusError)
			panic(recovered)
		}
		if err != nil || selected == nil || selected.channel == nil {
			handle.Finish(requesttrace.StatusError)
		} else {
			handle.Finish(requesttrace.StatusSuccess)
		}
	}()
	return getInitialChannel(c, modelName, relayMode)
}

func getRetryChannelWithTrace(c *gin.Context, ctx context.Context, state *retryState) (selected *model.Channel, err error) {
	handle := middleware.BeginRequestTraceStage(c, requesttrace.StageChannelSelection, requesttrace.Attributes{})
	defer func() {
		if recovered := recover(); recovered != nil {
			handle.Finish(requesttrace.StatusError)
			panic(recovered)
		}
		if err != nil || selected == nil {
			handle.Finish(requesttrace.StatusError)
		} else {
			handle.Finish(requesttrace.StatusSuccess)
		}
	}()
	return getRetryChannel(ctx, state)
}
