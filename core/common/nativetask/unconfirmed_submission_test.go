package nativetask

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// refundingWallet reports the hold refunded once a settle succeeded, like the
// application wallet, so a settled task leaves recovery.
type refundingWallet struct {
	walletStub
	refunded bool
}

func (w *refundingWallet) Prepayment(ctx context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	r, err := w.walletStub.Prepayment(ctx, c)
	if err == nil && c.Action == "settle" {
		w.refunded = true
	}
	r.Status = "pending"
	if w.refunded {
		r.Status = "refunded"
	}
	return r, err
}

// unconfirmed stores task "req" in the given never-confirmed state: an
// uncertain provider answer (submission_unknown), or a process that died
// mid-call (submitting). It returns the wallet with its history cleared.
func unconfirmed(t *testing.T, status string) (*Engine, *refundingWallet, *providerStub) {
	t.Helper()
	e, plan, _, p := setup(t)
	w := &refundingWallet{}
	e.Wallet = w
	if status == "submission_unknown" {
		p.err = errors.New("connection lost after write")
		task, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
		require.ErrorIs(t, err, ErrPending)
		require.Equal(t, "submission_unknown", task.Status)
	} else {
		_, _, err := model.ReserveNativeTask(e.DB, model.NativeTask{ID: "req", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "fp", OutputSchema: `{}`, FrozenContract: string(plan.Contract), BillingOperationID: "native:req"})
		require.NoError(t, err)
		require.NoError(t, model.TransitionNativeSubmission(e.DB, "req", "g", 1, "reserved", "submitting", ""))
	}
	w.commands = nil
	return e, w, p
}

// neverPoll fails the test if recovery tries to reach the provider: a task
// without an upstream ID has nothing to poll or cancel.
func neverPoll(t *testing.T) ResolvePoller {
	return func(context.Context, *model.NativeTask) (Poller, error) {
		t.Fatal("must not poll or cancel a task without an upstream ID")
		return nil, nil
	}
}

// Owner rule 2026-10-08: a submission fal never confirmed fails 15 minutes
// after it started with generation_timeout and is refunded in full in one
// wallet call. It is never polled, cancelled or resubmitted.
func TestUnconfirmedSubmissionFailsAndRefundsAtTheDeadline(t *testing.T) {
	for _, status := range []string{"submission_unknown", "submitting"} {
		t.Run(status, func(t *testing.T) {
			e, w, p := unconfirmed(t, status)
			now := acceptedAgo(t, e, model.AsyncGenerationDeadline+time.Second)
			report, err := e.RecoverOnce(context.Background(), "worker", now, neverPoll(t), nil)
			require.NoError(t, err)
			require.Equal(t, 1, report.Expired)
			task, err := model.GetNativeTask(e.DB, "req", "g", 1)
			require.NoError(t, err)
			require.Equal(t, "failed", task.Status)
			require.Equal(t, model.AsyncGenerationTimeoutCode, task.ErrorCode)
			require.Empty(t, task.UpstreamID)
			require.True(t, task.BillingSettled)
			requireTimeoutRefund(t, &w.walletStub)
			actions := walletActions(&w.walletStub)
			require.Zero(t, countAction(actions, "accept_attempt"), "%v", actions)
			require.Zero(t, countAction(actions, "reject_attempt"), "%v", actions)
			require.LessOrEqual(t, p.calls, 1, "never resubmitted")

			recorder := httptest.NewRecorder()
			WritePublic(recorder, 200, task)
			var public struct {
				Status string `json:"status"`
				Error  struct{ Code, Message string }
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &public))
			require.Equal(t, "failed", public.Status)
			require.Equal(t, model.AsyncGenerationTimeoutCode, public.Error.Code)
			require.Equal(t, model.AsyncGenerationTimeoutMessage, public.Error.Message)

			// A settled failure is not claimed again.
			report, err = e.RecoverOnce(context.Background(), "worker", now.Add(time.Hour), neverPoll(t), nil)
			require.NoError(t, err)
			require.Zero(t, report.Claimed)
		})
	}
}

// Younger unconfirmed submissions are left alone, and their next check is
// pulled in to just past the deadline.
func TestUnconfirmedSubmissionIsCheckedAgainAtTheDeadline(t *testing.T) {
	e, w, _ := unconfirmed(t, "submission_unknown")
	now := acceptedAgo(t, e, model.AsyncGenerationDeadline-30*time.Second)
	report, err := e.RecoverOnce(context.Background(), "worker", now, neverPoll(t), nil)
	require.NoError(t, err)
	require.Zero(t, report.Expired)
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "submission_unknown", task.Status)
	require.False(t, refundedWith(&w.walletStub, "platform_failure"))
	deadline := task.CreatedAt.Add(model.AsyncGenerationDeadline)
	require.True(t, time.Unix(task.NextRecoveryAt, 0).After(deadline))
	require.LessOrEqual(t, task.NextRecoveryAt, deadline.Add(2*time.Second).Unix())
}

// The two production rows of 2026-10-08 were left submission_unknown by the
// old code with a backed-off next_recovery_at (at most 30 minutes ahead).
// After the deploy the existing claim picks them up and fails and refunds them.
func TestUnconfirmedSubmissionFromBeforeTheDeployIsPickedUp(t *testing.T) {
	e, w, _ := unconfirmed(t, "submission_unknown")
	now := acceptedAgo(t, e, 3*time.Hour)
	require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "req").Update("next_recovery_at", now.Add(30*time.Minute).Unix()).Error)
	report, err := e.RecoverOnce(context.Background(), "worker", now, neverPoll(t), nil)
	require.NoError(t, err)
	require.Zero(t, report.Claimed)
	report, err = e.RecoverOnce(context.Background(), "worker", now.Add(30*time.Minute+time.Second), neverPoll(t), nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Expired)
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, model.AsyncGenerationTimeoutCode, task.ErrorCode)
	requireTimeoutRefund(t, &w.walletStub)
}

// A refund the wallet could not take is retried by a later pass; the task
// stays failed and is never resubmitted.
func TestUnconfirmedSubmissionRefundIsRetried(t *testing.T) {
	e, w, p := unconfirmed(t, "submission_unknown")
	now := acceptedAgo(t, e, model.AsyncGenerationDeadline+time.Minute)
	w.fail = "settle"
	report, err := e.RecoverOnce(context.Background(), "worker", now, neverPoll(t), nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Expired)
	require.Equal(t, 1, report.Deferred)
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "failed", task.Status)
	require.False(t, task.BillingSettled)
	w.fail = ""
	_, err = e.RecoverOnce(context.Background(), "worker", now.Add(time.Hour), neverPoll(t), nil)
	require.NoError(t, err)
	task, err = model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.True(t, task.BillingSettled)
	require.Equal(t, model.AsyncGenerationTimeoutCode, task.ErrorCode)
	require.Zero(t, countAction(walletActions(&w.walletStub), "execution_finished"))
	require.LessOrEqual(t, p.calls, 1)
}

// The clock starts when the submission starts, not when the request ID was
// reserved: a reservation resumed hours later is never expired while its
// provider call may still be in flight.
func TestUnconfirmedSubmissionClockStartsAtSubmission(t *testing.T) {
	e, plan, _, _ := setup(t)
	_, _, err := model.ReserveNativeTask(e.DB, model.NativeTask{ID: "req", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "fp", OutputSchema: `{}`, FrozenContract: string(plan.Contract), BillingOperationID: "native:req"})
	require.NoError(t, err)
	acceptedAgo(t, e, 3*time.Hour)
	require.NoError(t, model.TransitionNativeSubmission(e.DB, "req", "g", 1, "reserved", "submitting", ""))
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now(), task.CreatedAt, 5*time.Second)
	expired, err := model.ExpireUnconfirmedNativeSubmission(e.DB, "req", "g", 1, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.False(t, expired)
}

// The rule never touches a task with an upstream ID (the 63feae1 rule owns
// those), a reservation that was never submitted, or another owner's task.
func TestExpireUnconfirmedNativeSubmissionScope(t *testing.T) {
	e, plan, _, _ := setup(t)
	_, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	now := acceptedAgo(t, e, time.Hour)
	expired, err := model.ExpireUnconfirmedNativeSubmission(e.DB, "req", "g", 1, now)
	require.NoError(t, err)
	require.False(t, expired, "accepted tasks keep the provider deadline")

	_, _, err = model.ReserveNativeTask(e.DB, model.NativeTask{ID: "reserved", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "fp", OutputSchema: `{}`, FrozenContract: string(plan.Contract), BillingOperationID: "native:reserved"})
	require.NoError(t, err)
	require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "reserved").Update("created_at", now.Add(-time.Hour)).Error)
	expired, err = model.ExpireUnconfirmedNativeSubmission(e.DB, "reserved", "g", 1, now)
	require.NoError(t, err)
	require.False(t, expired, "nothing was submitted")

	e2, w2, _ := unconfirmed(t, "submission_unknown")
	now = acceptedAgo(t, e2, time.Hour)
	expired, err = model.ExpireUnconfirmedNativeSubmission(e2.DB, "req", "other", 1, now)
	require.NoError(t, err)
	require.False(t, expired)
	expired, err = model.ExpireUnconfirmedNativeSubmission(e2.DB, "req", "g", 1, now)
	require.NoError(t, err)
	require.True(t, expired)
	expired, err = model.ExpireUnconfirmedNativeSubmission(e2.DB, "req", "g", 1, now)
	require.NoError(t, err)
	require.False(t, expired, "only one caller ends the task")
	require.Empty(t, w2.commands)
}
