package task

import (
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// requireFailedImageRefund checks that the poll itself refunded the hold in
// full, in one wallet call and without execution_finished (owner rules
// 2026-10-02 and 2026-10-08), with no prepayment recovery pass in between.
func requireFailedImageRefund(t *testing.T, wallet *deadlineWallet) {
	t.Helper()
	require.Equal(t, []string{"accept_attempt", "settle:failed/platform_failure", "get"}, wallet.list())
	saved, err := model.GetImageTask("deadline", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "failed", saved.Status)
	require.NotNil(t, saved.Billing)
	require.Equal(t, "refunded", saved.Billing.Status)
	require.True(t, saved.BillingSettled)
	var usage model.AsyncUsageInfo
	require.NoError(t, model.LogDB.First(&usage).Error)
	require.Equal(t, model.AsyncUsageStatusFailed, usage.Status)
}

// A provider failure found by the poll is refunded at once.
func TestImagePollRefundsProviderFailureImmediately(t *testing.T) {
	wallet := &deadlineWallet{}
	fal := &deadlineFal{status: "FAILED"}
	setupDeadlineTask(t, time.Minute, "in_progress", fal, wallet)
	processOneImageUsage(t.Context(), claimDeadlineUsage(t))
	requireFailedImageRefund(t, wallet)
	saved, err := model.GetImageTask("deadline", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "upstream_failed", saved.Error.Code)
	require.Zero(t, fal.cancelCount())
}

// A failure already stored (another worker saved it, or the outbox update was
// lost) is refunded on the next pass without polling the provider again.
func TestImagePollRefundsStoredFailureImmediately(t *testing.T) {
	wallet := &deadlineWallet{}
	fal := &deadlineFal{status: "IN_PROGRESS"}
	setupDeadlineTask(t, time.Minute, "failed", fal, wallet)
	processOneImageUsage(t.Context(), claimDeadlineUsage(t))
	requireFailedImageRefund(t, wallet)
	require.Empty(t, fal.calls, "a stored failure is never polled again")
}

// A failed task without a hold (legacy post-paid or internal) calls no wallet.
func TestImagePollFailureWithoutPrepaymentCallsNoWallet(t *testing.T) {
	fal := &deadlineFal{status: "FAILED"}
	setupDeadlineTask(t, time.Minute, "in_progress", fal, nil)
	processOneImageUsage(t.Context(), claimDeadlineUsage(t))
	saved, err := model.GetImageTask("deadline", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "failed", saved.Status)
	require.Nil(t, saved.Billing)
	require.False(t, saved.BillingSettled)
}
