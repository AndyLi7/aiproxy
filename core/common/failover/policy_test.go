package failover

import (
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	now := time.Now()
	good := Failure{Acceptance: NotAccepted, Class: Transient}
	for _, tc := range []struct {
		name string
		f    Failure
		p    Policy
		s    State
		want bool
	}{
		{"safe", good, Policy{MaxRetries: 2}, State{}, true},
		{"unknown", Failure{Acceptance: Unknown, Class: Transient}, Policy{MaxRetries: 2}, State{}, false},
		{"accepted", Failure{Acceptance: Accepted, Class: Transient}, Policy{MaxRetries: 2}, State{}, false},
		{"invalid", Failure{Acceptance: NotAccepted, Class: InvalidRequest}, Policy{MaxRetries: 2}, State{}, false},
		{"disabled", good, Policy{}, State{}, false},
		{"exhausted", good, Policy{MaxRetries: 2}, State{Retries: 2}, false},
		{"deadline", good, Policy{MaxRetries: 2, Deadline: now}, State{}, false},
		{"pinned", good, Policy{MaxRetries: 2}, State{Pinned: true}, false},
		{"written", good, Policy{MaxRetries: 2}, State{Written: true}, false},
		{"cancelled", good, Policy{MaxRetries: 2}, State{Cancelled: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decide(tc.f, tc.p, tc.s, now); got.Retry != tc.want {
				t.Fatalf("%+v", got)
			}
		})
	}
}
