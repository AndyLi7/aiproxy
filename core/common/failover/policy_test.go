package failover_test

import (
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/failover"
)

func TestDecide(t *testing.T) {
	now := time.Now()

	good := failover.Failure{Acceptance: failover.NotAccepted, Class: failover.Transient}
	for _, tc := range []struct {
		name string
		f    failover.Failure
		p    failover.Policy
		s    failover.State
		want bool
	}{
		{"safe", good, failover.Policy{MaxRetries: 2}, failover.State{}, true},
		{"unknown", failover.Failure{Acceptance: failover.Unknown, Class: failover.Transient}, failover.Policy{MaxRetries: 2}, failover.State{}, false},
		{"accepted", failover.Failure{Acceptance: failover.Accepted, Class: failover.Transient}, failover.Policy{MaxRetries: 2}, failover.State{}, false},
		{"invalid", failover.Failure{Acceptance: failover.NotAccepted, Class: failover.InvalidRequest}, failover.Policy{MaxRetries: 2}, failover.State{}, false},
		{"disabled", good, failover.Policy{}, failover.State{}, false},
		{"exhausted", good, failover.Policy{MaxRetries: 2}, failover.State{Retries: 2}, false},
		{"deadline", good, failover.Policy{MaxRetries: 2, Deadline: now}, failover.State{}, false},
		{"pinned", good, failover.Policy{MaxRetries: 2}, failover.State{Pinned: true}, false},
		{"written", good, failover.Policy{MaxRetries: 2}, failover.State{Written: true}, false},
		{"cancelled", good, failover.Policy{MaxRetries: 2}, failover.State{Cancelled: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := failover.Decide(tc.f, tc.p, tc.s, now); got.Retry != tc.want {
				t.Fatalf("%+v", got)
			}
		})
	}
}
