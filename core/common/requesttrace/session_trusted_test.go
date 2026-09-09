package requesttrace

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSessionTrustedBindingPreservesBufferedAndFutureLifecycle(t *testing.T) {
	var saved []Span
	s := NewSession("request-a", func(span Span) bool { saved = append(saved, span); return true })
	auth := s.Begin(StageAuthentication, Attributes{})
	verified := VerifiedContext{claims: TrustedClaims{GroupID: "group-a", RequestID: "request-a", TraceID: "11111111111111111111111111111111", ParentSpanID: "22222222222222222222222222222222"}}
	require.Empty(t, saved)
	require.True(t, s.BindVerifiedContext("group-a", verified))
	require.Equal(t, verified.TraceID(), s.TraceID())
	auth.Finish(StatusSuccess)
	s.Begin(StageValidation, Attributes{}).Finish(StatusSuccess)
	s.Finish(StatusSuccess)
	require.Len(t, saved, 6)
	for _, span := range saved {
		require.NoError(t, Validate(span))
		require.Equal(t, "group-a", span.GroupID)
		require.Equal(t, verified.TraceID(), span.TraceID)
		if span.Stage == StageRequest {
			require.Equal(t, verified.ParentSpanID(), span.ParentSpanID)
		} else {
			require.Equal(t, s.RootID(), span.ParentSpanID)
		}
	}
	require.Equal(t, saved[0].StartedAt, saved[5].StartedAt)
}

func TestSessionStoredTaskUsesIndependentHTTPRoot(t *testing.T) {
	var saved []Span
	s := NewSession("poll-request", func(span Span) bool { saved = append(saved, span); return true })
	require.True(t, s.BindStoredTask("group-a", "11111111111111111111111111111111", "22222222222222222222222222222222"))
	s.Finish(StatusSuccess)
	require.Len(t, saved, 2)
	require.Equal(t, "poll-request", saved[0].RequestID)
	require.Equal(t, "22222222222222222222222222222222", saved[0].ParentSpanID)
	require.NotEqual(t, saved[0].SpanID, saved[0].ParentSpanID)
}
