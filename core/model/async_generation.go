package model

import "time"

// Shared leaf for every async generation lane (native /v1/model-tasks and
// image /v1/images/tasks). It holds only the rule, so both lanes import the
// same values; keep it free of lane-specific code.

// AsyncGenerationDeadline is the owner rule of 2026-10-08 for every model and
// provider: an accepted task with no result this long after the provider
// accepted it is failed with AsyncGenerationTimeoutCode, refunded in full (one
// wallet settle failed/platform_failure, no execution_finished) and cancelled
// upstream on a best-effort basis. A submission the provider never confirmed
// (no upstream ID: its outcome was unknown, or the submitting process died)
// fails the same way this long after the submission started; it has no ID to
// cancel and is never resubmitted. A later provider bill is platform loss.
// Never key this on a model. The application repeats the value in its docs and
// client poll windows.
const AsyncGenerationDeadline = 15 * time.Minute

// AsyncGenerationTimeoutCode is the public task error code for that failure.
// It is never a wallet reason: the wallet outcome stays platform_failure.
const AsyncGenerationTimeoutCode = "generation_timeout"

// AsyncGenerationTimeoutMessage is the client-facing sentence for that code.
// Retrying with the same X-Request-Id replays the failed task, so it asks for
// a new one.
const AsyncGenerationTimeoutMessage = "The provider did not finish this task within 15 minutes, so it was stopped. You were not charged; submit a new task with a new X-Request-Id."

// UpstreamUnavailableCode is the public task error code for a submission the
// provider refused for reasons on its or the platform's side, not the
// customer's input: authentication (401), payment or an exhausted provider
// balance (402, 403), an unknown endpoint (404) or throttling (429). It also
// covers a submission whose connection to the provider failed before the
// request was sent (a DNS or connect error; provider status 0 in the gateway
// log). Owner rule 2026-10-08 for every model and both async lanes: the
// provider created no request, so the task fails at once and the hold is
// refunded in full (wallet reason not_accepted on the native lane). Like
// AsyncGenerationTimeoutCode it is never a wallet reason.
const UpstreamUnavailableCode = "upstream_unavailable"

// UpstreamUnavailableMessage is the client-facing sentence for that code. A
// retry needs a new X-Request-Id: the same one replays the failed task.
const UpstreamUnavailableMessage = "The provider is temporarily unavailable, so this task was not started. You were not charged; try again later with a new X-Request-Id."

// InvalidParametersCode is the public task error code, on both async lanes,
// for input the provider rejected while naming the fields. Its issues carry
// only field paths and rule codes, never provider text or the customer's
// values. On the native lane (owner decision 2026-10-09) it is refunded in
// full: like upstream_rejected when the provider never accepted the request,
// like upstream_result_rejected when it rejected the input after acceptance.
const InvalidParametersCode = "invalid_parameters"

// PublicTaskErrorMessage is the fixed client-facing message of a
// platform-owned task failure code that customers see as is, or "" for every
// other code. Provider or stored text is never echoed for these codes.
func PublicTaskErrorMessage(code string) string {
	switch code {
	case AsyncGenerationTimeoutCode:
		return AsyncGenerationTimeoutMessage
	case UpstreamUnavailableCode:
		return UpstreamUnavailableMessage
	}
	return ""
}
