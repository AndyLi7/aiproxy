package nativetask

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestNativeRecoveryLeaseAndRestart(t *testing.T) {
	e, plan, _, provider := setup(t)
	ctx := context.Background()
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	now := time.Now()
	tasks, err := model.ClaimNativeRecovery(e.DB, "worker-a", now, 20)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	tasks, err = model.ClaimNativeRecovery(e.DB, "worker-b", now, 20)
	require.NoError(t, err)
	require.Empty(t, tasks)
	require.Error(t, model.ReleaseNativeRecovery(e.DB, "req", "worker-b", now))
	poll := &pollStub{result: nativeresult.PollResult{Status: "result_received", Output: json.RawMessage(`{"labels":[]}`)}}
	resolver := func(context.Context, *model.NativeTask) (Poller, error) { return poll, nil }
	report, err := e.RecoverOnce(ctx, "worker-b", now.Add(3*time.Minute), resolver, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Advanced)
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "completed", task.Status)
	require.Equal(t, 1, provider.calls)
	report, err = e.RecoverOnce(ctx, "worker-c", now.Add(3*time.Minute), resolver, nil)
	require.NoError(t, err)
	require.Zero(t, report.Claimed)
}
func TestNativeRecoveryNeverRetriesUnknownSubmission(t *testing.T) {
	e, plan, _, provider := setup(t)
	ctx := context.Background()
	_, _, err := model.ReserveNativeTask(e.DB, model.NativeTask{ID: "req", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "fp", OutputSchema: `{}`, FrozenContract: string(plan.Contract), BillingOperationID: "native:req"})
	require.NoError(t, err)
	require.NoError(t, model.TransitionNativeSubmission(e.DB, "req", "g", 1, "reserved", "submitting", ""))
	require.NoError(t, model.TransitionNativeSubmission(e.DB, "req", "g", 1, "submitting", "submission_unknown", "submission_outcome_unknown"))
	report, err := e.RecoverOnce(ctx, "worker", time.Now(), func(context.Context, *model.NativeTask) (Poller, error) {
		t.Fatal("must not poll unknown identity")
		return nil, nil
	}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Advanced)
	require.Zero(t, provider.calls)
}

func TestNativeRecoveryBacksOffWithAge(t *testing.T) {
	require.Equal(t, 2*time.Second, recoveryDelay(5*time.Second, false))
	require.Equal(t, 7500*time.Millisecond, recoveryDelay(time.Minute, false))
	require.Equal(t, time.Minute, recoveryDelay(time.Minute, true))
	require.Equal(t, 15*time.Minute, recoveryDelay(2*time.Hour, true))
	require.Equal(t, 30*time.Minute, recoveryDelay(48*time.Hour, false))
}

// Waiting for the provider ends at model.AsyncGenerationDeadline
// (generation_deadline_test.go). NativeTaskDeadline remains the backstop for a
// received result that can never be delivered: it fails as
// upstream_result_rejected and is refunded in full.
func TestNativeRecoveryExpiresUndeliverableResultsAfterLongDeadline(t *testing.T) {
	e, plan, _, _ := setup(t)
	ctx := context.Background()
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.NoError(t, model.SaveNativeTaskResult(e.DB, "req", "g", 1, []byte(`{"labels":[]}`)))
	now := time.Now()
	require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "req").Update("created_at", now.Add(-NativeTaskDeadline-time.Minute)).Error)
	once := &failOnceWallet{action: "execution_finished"}
	e.Wallet = once
	_, err = e.RecoverOnce(ctx, "worker", now, func(context.Context, *model.NativeTask) (Poller, error) {
		return nil, errors.New("a received result is never polled again")
	}, nil)
	require.NoError(t, err)
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "failed", task.Status)
	require.Equal(t, "upstream_result_rejected", task.ErrorCode)
	require.True(t, refundedWith(&once.walletStub, "platform_failure"))
}

// A received result younger than the long deadline keeps being retried.
func TestNativeRecoveryRetriesUndeliveredResultsBeforeLongDeadline(t *testing.T) {
	e, plan, w, _ := setup(t)
	ctx := context.Background()
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.NoError(t, model.SaveNativeTaskResult(e.DB, "req", "g", 1, []byte(`{"labels":[]}`)))
	now := time.Now()
	require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "req").Update("created_at", now.Add(-NativeTaskDeadline+time.Minute)).Error)
	w.fail = "execution_finished"
	report, err := e.RecoverOnce(ctx, "worker", now, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Deferred)
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "result_received", task.Status)
	require.False(t, refundedWith(w, "platform_failure"))
}

// failOnceWallet fails one wallet action the first time it is called.
type failOnceWallet struct {
	walletStub
	action string
	done   bool
}

func (f *failOnceWallet) Prepayment(ctx context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	if c.Action == f.action && !f.done {
		f.done = true
		f.commands = append(f.commands, c)
		return balance.PrepaymentReceipt{}, errors.New("wallet timeout")
	}
	return f.walletStub.Prepayment(ctx, c)
}
