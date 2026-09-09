package requesttrace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

const Version = 1

type Service string

const (
	ServiceAIProxy Service = "aiproxy"
	ServiceApp     Service = "app"
)

type Stage string

const (
	StageRequest             Stage = "request"
	StageInputProcessing     Stage = "input_processing"
	StageGatewayCall         Stage = "gateway_call"
	StageResultStorage       Stage = "result_storage"
	StageSettlement          Stage = "settlement"
	StageResponse            Stage = "response"
	StageAuthentication      Stage = "authentication"
	StageBalanceCheck        Stage = "balance_check"
	StageValidation          Stage = "validation"
	StageModelResolution     Stage = "model_resolution"
	StageChannelSelection    Stage = "channel_selection"
	StageUpstreamAttempt     Stage = "upstream_attempt"
	StageAsyncSubmit         Stage = "async_submit"
	StageAsyncStatusPoll     Stage = "async_status_poll"
	StageAsyncObservedResult Stage = "async_observed_result"
	StageAsyncResultFetch    Stage = "async_result_fetch"
	StageAsyncWait           Stage = "async_wait"
)

type Status string

const (
	StatusRunning   Status = "running"
	StatusSuccess   Status = "success"
	StatusError     Status = "error"
	StatusTimeout   Status = "timeout"
	StatusCancelled Status = "cancelled"
	StatusUnknown   Status = "unknown"
)

// Attributes is the complete whitelist of metadata accepted for a span.
// Optional scalar pointers distinguish an absent value from a measured zero.
type Attributes struct {
	PublicModelID   string   `json:"public_model_id,omitempty"`
	UpstreamModelID string   `json:"upstream_model_id,omitempty"`
	ChannelID       *int     `json:"channel_id,omitempty"`
	Attempt         *int     `json:"attempt,omitempty"`
	ErrorCode       string   `json:"error_code,omitempty"`
	HTTPStatus      *int     `json:"http_status,omitempty"`
	Width           *int     `json:"width,omitempty"`
	Height          *int     `json:"height,omitempty"`
	Seconds         *float64 `json:"seconds,omitempty"`
	AspectRatio     string   `json:"aspect_ratio,omitempty"`
	GenerateAudio   *bool    `json:"generate_audio,omitempty"`
}

func (a *Attributes) UnmarshalJSON(data []byte) error {
	type wire Attributes
	var decoded wire
	if err := unmarshalStrict(data, &decoded); err != nil {
		return err
	}
	*a = Attributes(decoded)
	return nil
}

// Span is a versioned snapshot of one bounded request-trace stage.
type Span struct {
	Version      int        `json:"version"`
	TraceID      string     `json:"trace_id"`
	SpanID       string     `json:"span_id"`
	ParentSpanID string     `json:"parent_span_id,omitempty"`
	RequestID    string     `json:"request_id"`
	GroupID      string     `json:"group_id,omitempty"`
	Service      Service    `json:"service"`
	Stage        Stage      `json:"stage"`
	Status       Status     `json:"status"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	DurationMS   *float64   `json:"duration_ms,omitempty"`
	Revision     int        `json:"revision"`
	Truncated    bool       `json:"truncated,omitempty"`
	Attributes   Attributes `json:"attributes"`
}

func (s *Span) UnmarshalJSON(data []byte) error {
	type wire Span
	var decoded wire
	if err := unmarshalStrict(data, &decoded); err != nil {
		return err
	}
	*s = Span(decoded)
	return nil
}

func unmarshalStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("requesttrace: trailing JSON value")
		}
		return err
	}
	return nil
}
