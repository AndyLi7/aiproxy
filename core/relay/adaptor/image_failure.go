package adaptor

import (
	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/model"
)

// ImageSubmissionFailure contains safe evidence, never upstream bodies or credentials.
type ImageSubmissionFailure struct {
	Failure     failover.Failure
	PublicError *model.ImageTaskError
}

func (e *ImageSubmissionFailure) Error() string {
	return "image submission failed: " + e.Failure.Evidence
}
func (e *ImageSubmissionFailure) FailoverFailure() failover.Failure { return e.Failure }
func (e *ImageSubmissionFailure) Is(target error) bool {
	return target == ErrImageSubmissionRejected && e.Failure.Acceptance == failover.NotAccepted
}
