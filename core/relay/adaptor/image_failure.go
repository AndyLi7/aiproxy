package adaptor

import (
	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/model"
)

// ImageSubmissionFailure contains safe evidence, never upstream bodies or credentials.
// ProviderStatus and ProviderReason are operator evidence for server logs only:
// they are never returned to customers and never persisted with a task.
type ImageSubmissionFailure struct {
	Failure     failover.Failure
	PublicError *model.ImageTaskError
	// ProviderStatus is the provider's HTTP status, or 0 when no response arrived.
	ProviderStatus int
	// ProviderReason is a short sanitized summary (see ProviderErrorReason).
	ProviderReason string
}

func (e *ImageSubmissionFailure) Error() string {
	return "image submission failed: " + e.Failure.Evidence
}
func (e *ImageSubmissionFailure) FailoverFailure() failover.Failure { return e.Failure }
func (e *ImageSubmissionFailure) Is(target error) bool {
	return target == ErrImageSubmissionRejected && e.Failure.Acceptance == failover.NotAccepted
}
