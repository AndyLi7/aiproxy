package adaptor

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/model"
)

// Owner rule 2026-10-08, for every model and both async lanes: a provider's
// answer to a paid submission falls into one of three classes.
//
//   - Input rejection (400, 413, 422, 451): the provider refused the customer's
//     input and created no request. Not accepted; never failed over.
//   - Provider unavailable (401, 402, 403, 404, 429, or the connection to the
//     provider failed before the request was sent): the provider refused for
//     reasons on its or the platform's side (credential, payment or exhausted
//     account balance, unknown endpoint, throttling) or was never reached, and
//     created no request. Not accepted; the task fails at once with
//     model.UpstreamUnavailableCode and is refunded in full, unless the lane
//     fails over to another channel.
//   - Anything else (5xx, 3xx, 408, transport errors after a connection):
//     acceptance is unknown. No retry; the task waits for reconciliation and
//     fails at model.AsyncGenerationDeadline if never confirmed.

// InputRejectionStatus reports provider statuses that reject the customer's input.
func InputRejectionStatus(status int) bool {
	switch status {
	case 400, 413, 422, 451:
		return true
	}
	return false
}

// ProviderUnavailableStatus reports provider statuses that prove the provider
// created no request for reasons that are not the customer's input.
func ProviderUnavailableStatus(status int) bool {
	switch status {
	case 401, 402, 403, 404, 429:
		return true
	}
	return false
}

// UpstreamUnavailableError is a fresh copy of the fixed public error.
func UpstreamUnavailableError() *model.ImageTaskError {
	return &model.ImageTaskError{Code: model.UpstreamUnavailableCode, Message: model.UpstreamUnavailableMessage}
}

// NewSubmissionRejected is the not-accepted failure of an input rejection.
// public, when set, is the structured public error (for example the fields a
// 422 named); callers otherwise use their lane's generic rejection.
func NewSubmissionRejected(status int, reason string, public *model.ImageTaskError) *ImageSubmissionFailure {
	return &ImageSubmissionFailure{
		Failure:        ErrImageSubmissionRejected.Failure,
		PublicError:    public,
		ProviderStatus: status,
		ProviderReason: reason,
	}
}

// NewProviderUnavailable is the not-accepted failure of a
// ProviderUnavailableStatus response. It is Transient, so a lane that fails
// over on a confirmed non-acceptance may try its next channel: another
// provider account can still work.
func NewProviderUnavailable(status int, reason string) *ImageSubmissionFailure {
	return &ImageSubmissionFailure{
		Failure: failover.Failure{
			Acceptance: failover.NotAccepted,
			Class:      failover.Transient,
			Evidence:   fmt.Sprintf("provider_unavailable_http_%d", status),
		},
		PublicError:    UpstreamUnavailableError(),
		ProviderStatus: status,
		ProviderReason: reason,
	}
}

// NewSubmissionUnknown is a provider answer that does not settle acceptance.
func NewSubmissionUnknown(status int, reason string) *ImageSubmissionFailure {
	return &ImageSubmissionFailure{
		Failure: failover.Failure{
			Acceptance: failover.Unknown,
			Class:      failover.UnknownFailure,
			Evidence:   fmt.Sprintf("provider_http_%d", status),
		},
		ProviderStatus: status,
		ProviderReason: reason,
	}
}

// ProviderUnavailable reports a confirmed non-acceptance that is not about the
// customer's input: a ProviderUnavailableStatus response, or a connection that
// failed before any byte reached the provider. Customers see
// model.UpstreamUnavailableCode for it, never a rejection of their input.
func ProviderUnavailable(err error) bool {
	var failure *ImageSubmissionFailure
	return errors.As(err, &failure) &&
		failure.Failure.Acceptance == failover.NotAccepted &&
		failure.Failure.Class == failover.Transient
}

// NotAcceptedTaskError is the task error stored for a submission the provider
// did not accept: the adaptor's structured public error when it has one, the
// fixed upstream_unavailable error when the provider was unavailable, else
// the lane's generic rejection.
func NotAcceptedTaskError(err error) *model.ImageTaskError {
	var failure *ImageSubmissionFailure
	if errors.As(err, &failure) && failure.PublicError != nil {
		return failure.PublicError
	}
	if ProviderUnavailable(err) {
		return UpstreamUnavailableError()
	}
	return &model.ImageTaskError{Code: "submission_rejected", Message: "Upstream rejected image submission"}
}

// SubmissionEvidence returns the provider status and reason carried by err,
// for server logs. Errors without evidence report status 0 and a fixed text:
// raw error strings may contain credential URLs and are never logged.
func SubmissionEvidence(err error) (int, string) {
	var failure *ImageSubmissionFailure
	if !errors.As(err, &failure) {
		return 0, "no provider evidence"
	}
	reason := failure.ProviderReason
	if reason == "" {
		reason = failure.Failure.Evidence
	}
	return failure.ProviderStatus, reason
}

// providerReasonLimit bounds a reason in server logs, in bytes.
const providerReasonLimit = 200

// ProviderErrorReason summarizes a provider error response for server logs
// only. Account and availability errors concern the platform's provider
// account, so their message text is kept: the JSON field detail, message,
// error or error.message, else the raw body. Input rejections (400, 413, 422,
// 451) may quote the customer's prompt, so only structured error types, codes
// and field paths are kept for them. Control characters are removed, every
// secret passed in (the request's credential) is redacted, and the result is
// cut to about 200 bytes. Request bodies and headers are never included.
func ProviderErrorReason(status int, body []byte, secrets ...string) string {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	structured := structuredProviderReason(fields)
	reason := structured
	if !InputRejectionStatus(status) {
		if text := providerMessage(fields); text != "" {
			reason = text
		} else if reason == "" {
			reason = string(body)
		}
	}
	reason = strings.Join(strings.FieldsFunc(reason, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == utf8.RuneError
	}), " ")
	for _, secret := range secrets {
		if len(secret) >= 8 {
			reason = strings.ReplaceAll(reason, secret, "[redacted]")
		}
	}
	if len(reason) > providerReasonLimit {
		cut := providerReasonLimit
		for cut > 0 && !utf8.RuneStart(reason[cut]) {
			cut--
		}
		reason = reason[:cut] + "..."
	}
	if reason == "" {
		if InputRejectionStatus(status) {
			return "input rejected (details withheld)"
		}
		return "no error message"
	}
	return reason
}

// providerMessage is the free-text message of a JSON error body.
func providerMessage(fields map[string]json.RawMessage) string {
	for _, name := range []string{"detail", "message", "error"} {
		var text string
		if json.Unmarshal(fields[name], &text) == nil && text != "" {
			return text
		}
	}
	var nested struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(fields["error"], &nested) == nil {
		return nested.Message
	}
	return ""
}

// structuredProviderReason keeps only machine-readable parts of an error body:
// FastAPI/pydantic detail types with their field paths, and error codes with
// their parameter names. Values, messages and echoed input are dropped.
func structuredProviderReason(fields map[string]json.RawMessage) string {
	var parts []string
	var details []struct {
		Type string `json:"type"`
		Loc  []any  `json:"loc"`
	}
	if json.Unmarshal(fields["detail"], &details) == nil {
		for _, detail := range details {
			if len(parts) == 4 {
				break
			}
			path := make([]string, 0, len(detail.Loc))
			for _, part := range detail.Loc {
				path = append(path, reasonToken(fmt.Sprint(part)))
			}
			parts = append(parts, reasonToken(detail.Type)+"@"+strings.Join(path, "."))
		}
	}
	var coded struct {
		Code  string `json:"code"`
		Type  string `json:"type"`
		Param string `json:"param"`
	}
	if json.Unmarshal(fields["error"], &coded) == nil && (coded.Code != "" || coded.Type != "") {
		part := coded.Code
		if part == "" {
			part = coded.Type
		}
		part = reasonToken(part)
		if coded.Param != "" {
			part += "@" + reasonToken(coded.Param)
		}
		parts = append(parts, part)
	}
	var code string
	if json.Unmarshal(fields["code"], &code) == nil && code != "" {
		parts = append(parts, reasonToken(code))
	}
	return strings.Join(parts, ", ")
}

// reasonToken keeps identifier-like values (types, codes, field names and
// indexes) and replaces anything else, which could be echoed input, with "*".
func reasonToken(value string) string {
	if value == "" || len(value) > 64 {
		return "*"
	}
	for _, r := range value {
		if !(r == '_' || r == '-' || r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return "*"
		}
	}
	return value
}
