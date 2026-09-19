package failover

import "testing"

func TestAttemptAcceptanceIsSticky(t *testing.T) {
	safe := Failure{Acceptance: NotAccepted, Class: Transient}
	for _, tc := range []struct {
		name  string
		prior Acceptance
		want  Acceptance
	}{
		{"first unsent", NotAccepted, NotAccepted},
		{"prior response", Accepted, Unknown},
		{"prior uncertain", Unknown, Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Attempt{}
			a.BeginCall()
			a.Observe(Failure{Acceptance: tc.prior})
			a.BeginCall()
			got := a.Observe(safe)
			if got.Acceptance != tc.want {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestSingleCallExplicitRejectionRemainsUsable(t *testing.T) {
	a := &Attempt{}
	a.BeginCall()
	a.Observe(Failure{Acceptance: Unknown}) // HTTP response before protocol parsing.
	f := a.Observe(Failure{Acceptance: NotAccepted, Class: Transient})
	if f.Acceptance != NotAccepted {
		t.Fatalf("%+v", f)
	}
}
