package nativetask

import (
	"context"
	"encoding/json"
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
