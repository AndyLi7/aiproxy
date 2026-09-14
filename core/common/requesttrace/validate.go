package requesttrace

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxRequestIDBytes   = 128
	maxGroupIDBytes     = 64
	maxModelIDBytes     = 128
	maxErrorCodeBytes   = 64
	maxAspectRatioBytes = 32
	maxAttributesBytes  = 2048
	maxSpansPerRecorder = 256
)

var (
	errAttributesTooLarge = errors.New("requesttrace: attributes exceed 2048 bytes")
	errorCodePattern      = regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)*$`)
	aspectRatioPattern    = regexp.MustCompile(`^(?:adaptive|[1-9][0-9]{0,4}:[1-9][0-9]{0,4})$`)
)

// Validate rejects values outside the versioned, safe trace contract.
func Validate(span Span) error {
	if span.Version != Version {
		return fmt.Errorf("requesttrace: unsupported version %d", span.Version)
	}
	if !validRandomID(span.TraceID) {
		return fmt.Errorf("requesttrace: invalid trace ID")
	}
	if !validRandomID(span.SpanID) {
		return fmt.Errorf("requesttrace: invalid span ID")
	}
	if span.ParentSpanID != "" && !validRandomID(span.ParentSpanID) {
		return fmt.Errorf("requesttrace: invalid parent span ID")
	}
	if err := validateRequiredString("request ID", span.RequestID, maxRequestIDBytes); err != nil {
		return err
	}
	if err := validateOptionalString("group ID", span.GroupID, maxGroupIDBytes); err != nil {
		return err
	}
	if !validService(span.Service) {
		return fmt.Errorf("requesttrace: invalid service %q", span.Service)
	}
	if !validStage(span.Stage) {
		return fmt.Errorf("requesttrace: invalid stage %q", span.Stage)
	}
	if !validStatus(span.Status) {
		return fmt.Errorf("requesttrace: invalid status %q", span.Status)
	}
	if span.StartedAt.IsZero() || !isUTC(span.StartedAt) {
		return fmt.Errorf("requesttrace: started_at must be a non-zero UTC timestamp")
	}
	if span.EndedAt != nil && (span.EndedAt.IsZero() || !isUTC(*span.EndedAt)) {
		return fmt.Errorf("requesttrace: ended_at must be a non-zero UTC timestamp")
	}
	if span.Revision <= 0 {
		return fmt.Errorf("requesttrace: revision must be positive")
	}
	if err := validateLifecycle(span); err != nil {
		return err
	}
	if err := validateAttributes(span.Attributes); err != nil {
		return err
	}
	return nil
}

func validateLifecycle(span Span) error {
	switch span.Status {
	case StatusRunning, StatusUnknown:
		if span.EndedAt != nil || span.DurationMS != nil {
			return fmt.Errorf("requesttrace: %s span cannot have completion timing", span.Status)
		}
	default:
		if span.EndedAt == nil || span.DurationMS == nil {
			return fmt.Errorf("requesttrace: completed span requires end time and duration")
		}
		if math.IsNaN(*span.DurationMS) || math.IsInf(*span.DurationMS, 0) || *span.DurationMS < 0 {
			return fmt.Errorf("requesttrace: duration must be finite and non-negative")
		}
	}
	return nil
}

func validateAttributes(attributes Attributes) error {
	encoded, err := json.Marshal(attributes)
	if err != nil {
		return fmt.Errorf("requesttrace: encode attributes: %w", err)
	}
	if len(encoded) > maxAttributesBytes {
		return errAttributesTooLarge
	}
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{"public model ID", attributes.PublicModelID, maxModelIDBytes},
		{"upstream model ID", attributes.UpstreamModelID, maxModelIDBytes},
		{"error code", attributes.ErrorCode, maxErrorCodeBytes},
		{"aspect ratio", attributes.AspectRatio, maxAspectRatioBytes},
	} {
		if err := validateOptionalString(field.name, field.value, field.limit); err != nil {
			return err
		}
	}
	if attributes.ErrorCode != "" && !errorCodePattern.MatchString(attributes.ErrorCode) {
		return fmt.Errorf("requesttrace: error code must be lowercase snake_case")
	}
	if attributes.AspectRatio != "" && !aspectRatioPattern.MatchString(attributes.AspectRatio) {
		return fmt.Errorf("requesttrace: aspect ratio must be adaptive or a numeric colon ratio")
	}
	for _, field := range []struct {
		name  string
		value *int
	}{
		{"attempt", attributes.Attempt},
		{"channel ID", attributes.ChannelID},
		{"width", attributes.Width},
		{"height", attributes.Height},
	} {
		if field.value != nil && *field.value < 0 {
			return fmt.Errorf("requesttrace: %s must be non-negative", field.name)
		}
	}
	if attributes.HTTPStatus != nil && (*attributes.HTTPStatus < 100 || *attributes.HTTPStatus > 599) {
		return fmt.Errorf("requesttrace: HTTP status must be between 100 and 599")
	}
	if attributes.Seconds != nil && (math.IsNaN(*attributes.Seconds) || math.IsInf(*attributes.Seconds, 0) || *attributes.Seconds < 0) {
		return fmt.Errorf("requesttrace: seconds must be finite and non-negative")
	}
	return nil
}

func validRandomID(value string) bool {
	if len(value) != 32 {
		return false
	}
	decoded := make([]byte, 16)
	if _, err := hex.Decode(decoded, []byte(value)); err != nil {
		return false
	}
	for _, char := range value {
		if char >= 'A' && char <= 'F' {
			return false
		}
	}
	return true
}

func validateRequiredString(name, value string, limit int) error {
	if value == "" {
		return fmt.Errorf("requesttrace: %s is required", name)
	}
	return validateOptionalString(name, value, limit)
}

func validateOptionalString(name, value string, limit int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("requesttrace: %s is not valid UTF-8", name)
	}
	if len(value) > limit {
		return fmt.Errorf("requesttrace: %s exceeds %d bytes", name, limit)
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return fmt.Errorf("requesttrace: %s contains a control character", name)
		}
	}
	return nil
}

func validService(service Service) bool {
	return service == ServiceAIProxy || service == ServiceApp
}

func validStage(stage Stage) bool {
	switch stage {
	case StageRequest,
		StageInputProcessing,
		StageGatewayCall,
		StageResultStorage,
		StageSettlement,
		StageResponse,
		StageAuthentication,
		StageBalanceCheck,
		StageValidation,
		StageModelResolution,
		StageChannelSelection,
		StageUpstreamAttempt,
		StageAsyncSubmit,
		StageAsyncStatusPoll,
		StageAsyncObservedResult,
		StageAsyncResultFetch,
		StageAsyncWait:
		return true
	default:
		return false
	}
}

func validStatus(status Status) bool {
	switch status {
	case StatusRunning, StatusSuccess, StatusError, StatusTimeout, StatusCancelled, StatusUnknown:
		return true
	default:
		return false
	}
}

func isUTC(value time.Time) bool {
	_, offset := value.Zone()
	return offset == 0
}
