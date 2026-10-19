package failover_test

import (
	"testing"

	"github.com/labring/aiproxy/core/common/failover"
)

func TestAttemptAcceptanceIsSticky(t *testing.T) {
	safe := failover.Failure{Acceptance: failover.NotAccepted, Class: failover.Transient}
	for _, tc := range []struct {
		name  string
		prior failover.Acceptance
		want  failover.Acceptance
	}{
		{"first unsent", failover.NotAccepted, failover.NotAccepted},
		{"prior response", failover.Accepted, failover.Unknown},
		{"prior uncertain", failover.Unknown, failover.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &failover.Attempt{}
			a.BeginCall()
			a.Observe(failover.Failure{Acceptance: tc.prior})
			a.BeginCall()

			got := a.Observe(safe)
			if got.Acceptance != tc.want {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestSingleCallExplicitRejectionRemainsUsable(t *testing.T) {
	a := &failover.Attempt{}
	a.BeginCall()
	a.Observe(
		failover.Failure{Acceptance: failover.Unknown},
	) // HTTP response before protocol parsing.

	f := a.Observe(failover.Failure{Acceptance: failover.NotAccepted, Class: failover.Transient})
	if f.Acceptance != failover.NotAccepted {
		t.Fatalf("%+v", f)
	}
}
