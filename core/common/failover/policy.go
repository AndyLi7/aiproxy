// Package failover makes acceptance evidence the prerequisite for changing channels.
package failover

import (
	"errors"
	"time"
)

type Acceptance string

const (
	NotAccepted Acceptance = "not_accepted"
	Accepted    Acceptance = "accepted"
	Unknown     Acceptance = "unknown"
)

type Class string

const (
	Transient      Class = "transient"
	InvalidRequest Class = "invalid_request"
	Permanent      Class = "permanent"
	UnknownFailure Class = "unknown"
)

type Failure struct {
	Acceptance Acceptance
	Class      Class
	Evidence   string
}
type Policy struct {
	MaxRetries int
	Deadline   time.Time
}
type State struct {
	Retries                    int
	Pinned, Written, Cancelled bool
}
type Decision struct {
	Retry  bool
	Reason string
}

func Decide(f Failure, p Policy, s State, now time.Time) Decision {
	reason := ""
	switch {
	case s.Cancelled:
		reason = "cancelled"
	case s.Written:
		reason = "response_written"
	case s.Pinned:
		reason = "pinned"
	case f.Acceptance == Accepted:
		reason = "accepted"
	case f.Acceptance != NotAccepted:
		reason = "acceptance_unknown"
	case f.Class != Transient:
		reason = "non_retryable"
	case p.MaxRetries >= 0 && s.Retries >= p.MaxRetries:
		reason = "attempt_budget"
	case !p.Deadline.IsZero() && !now.Before(p.Deadline):
		reason = "time_budget"
	default:
		return Decision{Retry: true, Reason: "confirmed_not_accepted"}
	}
	return Decision{Reason: reason}
}
func FromError(err error) Failure {
	var provider interface{ FailoverFailure() Failure }
	if errors.As(err, &provider) {
		return provider.FailoverFailure()
	}
	return Failure{Acceptance: Unknown, Class: UnknownFailure}
}
