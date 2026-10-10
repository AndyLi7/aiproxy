package adaptor_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/stretchr/testify/require"
)

// Owner rule 2026-10-08: account and availability errors keep the provider's
// message (that is what operators need), input rejections never keep free text
// (it may quote the customer's prompt), and a credential is never logged.
func TestProviderErrorReasonIsSanitized(t *testing.T) {
	locked := []byte(`{"detail":"User is locked. Reason: Exhausted balance. Top up your balance at fal.ai/dashboard/billing."}`)
	require.Equal(t, "User is locked. Reason: Exhausted balance. Top up your balance at fal.ai/dashboard/billing.", adaptor.ProviderErrorReason(403, locked))

	ark := []byte(`{"error":{"code":"AccountOverdueError","message":"Your account has an overdue balance.","type":"Forbidden"}}`)
	require.Equal(t, "Your account has an overdue balance.", adaptor.ProviderErrorReason(403, ark))

	invalid := []byte(`{"detail":[{"type":"string_too_long","loc":["body","prompt"],"msg":"String should have at most 10 characters","input":"a private customer prompt"}]}`)
	reason := adaptor.ProviderErrorReason(422, invalid)
	require.Equal(t, "string_too_long@body.prompt", reason)
	require.NotContains(t, reason, "private")

	moderated := []byte(`{"detail":"Prompt 'a private customer prompt' was flagged"}`)
	require.Equal(t, "input rejected (details withheld)", adaptor.ProviderErrorReason(400, moderated))

	echoed := []byte("unauthorized: Authorization: Key fal-secret-key-123\nretry\tlater")
	reason = adaptor.ProviderErrorReason(401, echoed, "fal-secret-key-123")
	require.Equal(t, "unauthorized: Authorization: Key [redacted] retry later", reason)

	long := adaptor.ProviderErrorReason(502, []byte(strings.Repeat("é", 300)))
	require.LessOrEqual(t, len(long), 203)
	require.True(t, strings.HasSuffix(long, "..."))
	require.NotContains(t, long, "�", "never cut inside a character")

	require.Equal(t, "no error message", adaptor.ProviderErrorReason(503, nil))
}

func TestSubmissionClassesAndPublicErrors(t *testing.T) {
	for _, status := range []int{401, 402, 403, 404, 429} {
		require.True(t, adaptor.ProviderUnavailableStatus(status))
		require.False(t, adaptor.InputRejectionStatus(status))
		err := adaptor.NewProviderUnavailable(status, "reason")
		failure := failover.FromError(err)
		require.Equal(t, failover.NotAccepted, failure.Acceptance)
		require.Equal(t, failover.Transient, failure.Class, "a lane that fails over on non-acceptance may try another channel")
		require.True(t, adaptor.ProviderUnavailable(err))
		require.ErrorIs(t, err, adaptor.ErrImageSubmissionRejected, "not accepted")
		require.Equal(t, &model.ImageTaskError{Code: "upstream_unavailable", Message: model.UpstreamUnavailableMessage}, adaptor.NotAcceptedTaskError(err))
		gotStatus, gotReason := adaptor.SubmissionEvidence(err)
		require.Equal(t, status, gotStatus)
		require.Equal(t, "reason", gotReason)
	}
	for _, status := range []int{400, 413, 422, 451} {
		require.True(t, adaptor.InputRejectionStatus(status))
		err := adaptor.NewSubmissionRejected(status, "", nil)
		require.False(t, adaptor.ProviderUnavailable(err))
		require.Equal(t, "submission_rejected", adaptor.NotAcceptedTaskError(err).Code)
	}
	for _, status := range []int{302, 408, 500, 502, 503} {
		require.False(t, adaptor.InputRejectionStatus(status))
		require.False(t, adaptor.ProviderUnavailableStatus(status))
		err := adaptor.NewSubmissionUnknown(status, "")
		require.Equal(t, failover.Unknown, failover.FromError(err).Acceptance)
		require.NotErrorIs(t, err, adaptor.ErrImageSubmissionRejected)
	}
	// A connection that failed before reaching the provider is not the
	// customer's input either.
	unsent := &adaptor.ImageSubmissionFailure{Failure: failover.Failure{Acceptance: failover.NotAccepted, Class: failover.Transient}}
	require.Equal(t, "upstream_unavailable", adaptor.NotAcceptedTaskError(unsent).Code)
	status, reason := adaptor.SubmissionEvidence(errors.New("dial tcp: https://user:pass@host"))
	require.Zero(t, status)
	require.Equal(t, "no provider evidence", reason, "raw error text is never logged")
}
