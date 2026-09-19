package failover

import "sync/atomic"

// AttemptContextKey identifies server-owned per-channel-attempt evidence in Gin.
const AttemptContextKey = "aiproxy.failover.attempt_evidence"

// Attempt retains uncertainty across multiple upstream calls by one handler.
// A later pre-connection failure cannot prove an earlier operation was rejected.
type Attempt struct {
	uncertain atomic.Bool
	calls     atomic.Int64
}

func (a *Attempt) BeginCall() {
	if a != nil {
		a.calls.Add(1)
	}
}

func (a *Attempt) Observe(f Failure) Failure {
	if a == nil {
		return f
	}
	if f.Acceptance != NotAccepted {
		a.uncertain.Store(true)
	}
	if f.Acceptance == NotAccepted && a.calls.Load() > 1 && a.uncertain.Load() {
		f.Acceptance = Unknown
		f.Evidence = "prior_call_may_have_been_accepted"
	}
	return f
}
