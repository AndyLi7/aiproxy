package model

import (
	"net/http"

	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/mode"
)

func PublicVideoError(statusCode int) adaptor.Error {
	err := OpenAIError{
		Message: "The request could not be completed.",
		Type:    "api_error",
		Code:    "internal_error",
	}

	switch statusCode {
	case http.StatusBadRequest:
		err.Message = "The request contains an invalid parameter."
		err.Type = "invalid_request_error"
		err.Code = "invalid_parameter"
	case http.StatusUnauthorized:
		err.Message = "The API key is missing or invalid."
		err.Type = "authentication_error"
		err.Code = "invalid_api_key"
	case http.StatusForbidden:
		err.Message = "The API key cannot access this resource."
		err.Type = "permission_error"
		err.Code = "permission_denied"
	case http.StatusNotFound:
		err.Message = "The requested resource was not found."
		err.Type = "invalid_request_error"
		err.Code = "not_found"
	case http.StatusTooManyRequests:
		err.Message = "Too many requests. Please retry later."
		err.Type = "rate_limit_error"
		err.Code = "rate_limit_exceeded"
	default:
		if statusCode >= http.StatusBadRequest && statusCode < http.StatusInternalServerError {
			err.Type = "invalid_request_error"
			err.Code = "request_rejected"
		}
	}

	return NewOpenAIError(statusCode, err)
}

const (
	ErrorTypeAIPROXY     = "aiproxy_error"
	ErrorTypeUpstream    = "upstream_error"
	ErrorCodeBadResponse = "bad_response"
)

func WrapperError(
	m mode.Mode,
	statusCode int,
	err error,
	opts ...WrapperErrorOptionFunc,
) adaptor.Error {
	return WrapperErrorWithMessage(m, statusCode, err.Error(), opts...)
}

type WrapperErrorOption struct {
	Type string
	Code any
}

type WrapperErrorOptionFunc func(o *WrapperErrorOption)

func WithType(typ string) WrapperErrorOptionFunc {
	return func(o *WrapperErrorOption) {
		o.Type = typ
	}
}

func WithCode(code any) WrapperErrorOptionFunc {
	return func(o *WrapperErrorOption) {
		o.Code = code
	}
}

func DefaultWrapperErrorOption() WrapperErrorOption {
	return WrapperErrorOption{
		Type: ErrorTypeAIPROXY,
	}
}

func WrapperErrorWithMessage(
	m mode.Mode,
	statusCode int,
	message string,
	opts ...WrapperErrorOptionFunc,
) adaptor.Error {
	opt := DefaultWrapperErrorOption()
	for _, o := range opts {
		if o == nil {
			continue
		}

		o(&opt)
	}

	switch m {
	case mode.Anthropic:
		return NewAnthropicError(statusCode, AnthropicError{
			Message: message,
			Type:    opt.Type,
		})
	case mode.VideoGenerationsJobs,
		mode.VideoGenerationsGetJobs,
		mode.VideoGenerationsContent,
		mode.Videos,
		mode.VideosGet,
		mode.VideosContent,
		mode.VideosDelete,
		mode.VideosRemix,
		mode.VideosEdits,
		mode.VideosExtensions:
		return NewOpenAIVideoError(statusCode, OpenAIVideoError{
			Detail: message,
		})
	case mode.Gemini,
		mode.GeminiFiles,
		mode.GeminiVideo,
		mode.GeminiVideoOperations:
		return NewGeminiError(statusCode, GeminiError{
			Message: message,
			Status:  opt.Type,
			Code:    statusCode,
		})
	default:
		return NewOpenAIError(statusCode, OpenAIError{
			Message: message,
			Type:    opt.Type,
			Code:    opt.Code,
		})
	}
}
