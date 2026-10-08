package nativetask

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// cancelPoller is a provider poller that also accepts best-effort cancels.
type cancelPoller struct {
	pollStub
	wallet               *walletStub
	cancels              []string
	refundedBeforeCancel []bool
	cancelErr            error
}

func (p *cancelPoller) CancelNative(_ context.Context, endpoint, id string) (string, error) {
	p.cancels = append(p.cancels, endpoint+"|"+id)
	if p.wallet != nil {
		p.refundedBeforeCancel = append(p.refundedBeforeCancel, refundedWith(p.wallet, "platform_failure"))
	}
	if p.cancelErr != nil {
		return "", p.cancelErr
	}
	return "cancellation_requested", nil
}

// acceptedAgo moves the acceptance time of task "req" into the past.
func acceptedAgo(t *testing.T, e *Engine, age time.Duration) time.Time {
	t.Helper()
	now := time.Now()
	require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "req").Update("created_at", now.Add(-age)).Error)
	return now
}

// walletActions lists wallet calls, with settle outcomes spelled out.
func walletActions(w *walletStub) []string {
	actions := make([]string, 0, len(w.commands))
	for _, c := range w.commands {
		action := c.Action
		if c.Outcome != nil {
			action += ":" + c.Outcome.Kind + "/" + c.Outcome.Reason
		}
		actions = append(actions, action)
	}
	return actions
}

func lastIndex(actions []string, want string) int {
	for i := len(actions) - 1; i >= 0; i-- {
		if actions[i] == want {
			return i
		}
	}
	return -1
}

func countAction(actions []string, want string) int {
	n := 0
	for _, a := range actions {
		if a == want {
			n++
		}
	}
	return n
}

// requireTimeoutRefund checks the wallet contract the app relies on: one full
// platform_failure refund, exactly once, with no execution_finished (which
// would open a window for an actual-cost charge) and nothing delivered.
func requireTimeoutRefund(t *testing.T, w *walletStub) {
	t.Helper()
	actions := walletActions(w)
	settle := lastIndex(actions, "settle:failed/platform_failure")
	require.NotEqual(t, -1, settle, "the hold must be refunded in full: %v", actions)
	require.Equal(t, 1, countAction(actions, "settle:failed/platform_failure"), "%v", actions)
	require.Zero(t, countAction(actions, "execution_finished"), "a timeout refunds in one call: %v", actions)
	require.Zero(t, countAction(actions, "delivered"), "%v", actions)
	for _, a := range actions {
		require.NotContains(t, a, "generation_timeout", "the wallet reason stays platform_failure")
	}
}

// Owner rule 2026-10-08: an accepted task with no result 15 minutes after
// acceptance fails, whatever the last poll said: still queued, still running,
// or an error. The provider is polled once more first, then the customer is
// refunded, then fal is asked to cancel.
func TestGenerationDeadlineFailsRefundsAndCancelsWhateverThePollReturns(t *testing.T) {
	cases := map[string]pollStub{
		"queued":  {result: nativeresult.PollResult{Status: "queued"}},
		"running": {result: nativeresult.PollResult{Status: "running"}},
		"error":   {err: errors.New("provider unreachable")},
	}
	for name, stub := range cases {
		t.Run(name, func(t *testing.T) {
			e, plan, w, _ := setup(t)
			ctx := context.Background()
			_, err := e.Submit(ctx, "req", "g", 1, body, plan)
			require.NoError(t, err)
			now := acceptedAgo(t, e, model.AsyncGenerationDeadline+time.Second)
			poll := &cancelPoller{pollStub: stub, wallet: w}
			resolve := func(context.Context, *model.NativeTask) (Poller, error) { return poll, nil }
			report, err := e.RecoverOnce(ctx, "worker", now, resolve, nil)
			require.NoError(t, err)
			require.Equal(t, 1, report.Expired)
			require.Equal(t, 1, poll.calls, "the provider is polled once more before expiry")
			task, err := model.GetNativeTask(e.DB, "req", "g", 1)
			require.NoError(t, err)
			require.Equal(t, "failed", task.Status)
			require.Equal(t, model.AsyncGenerationTimeoutCode, task.ErrorCode)
			require.NotEmpty(t, task.BillingReceiptJSON)
			requireTimeoutRefund(t, w)
			require.Equal(t, []string{"fal-ai/vector/model|upstream-1"}, poll.cancels)
			require.Equal(t, []bool{true}, poll.refundedBeforeCancel, "cancel never delays the refund")
		})
	}
}

// Cancellation is best effort: a failed cancel is only logged.
func TestGenerationDeadlineCancelFailureNeverBlocksRefund(t *testing.T) {
	e, plan, w, _ := setup(t)
	ctx := context.Background()
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	now := acceptedAgo(t, e, 20*time.Minute)
	poll := &cancelPoller{pollStub: pollStub{result: nativeresult.PollResult{Status: "running"}}, cancelErr: errors.New("fal returned HTTP 500")}
	_, err = e.RecoverOnce(ctx, "worker", now, func(context.Context, *model.NativeTask) (Poller, error) { return poll, nil }, nil)
	require.NoError(t, err)
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "failed", task.Status)
	require.Equal(t, model.AsyncGenerationTimeoutCode, task.ErrorCode)
	requireTimeoutRefund(t, w)
	require.Len(t, poll.cancels, 1, "a cancel is sent once and never retried")
}

// A rotated or removed credential is never replaced by another channel's key:
// the task still fails and refunds, and no cancel is sent at all.
func TestGenerationDeadlineRotatedCredentialNeverCancels(t *testing.T) {
	e, plan, w, _ := setup(t)
	ctx := context.Background()
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	now := acceptedAgo(t, e, 20*time.Minute)
	resolves := 0
	resolve := func(context.Context, *model.NativeTask) (Poller, error) {
		resolves++
		return nil, ErrUnavailable
	}
	_, err = e.RecoverOnce(ctx, "worker", now, resolve, nil)
	require.NoError(t, err)
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "failed", task.Status)
	require.Equal(t, model.AsyncGenerationTimeoutCode, task.ErrorCode)
	requireTimeoutRefund(t, w)
	require.NotZero(t, resolves)
}

// One poll still happens at the deadline; a result the provider finished in
// time is delivered and charged normally, never expired or cancelled.
func TestGenerationDeadlineDeliversResultFoundAtTheDeadline(t *testing.T) {
	e, plan, w, _ := setup(t)
	ctx := context.Background()
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	now := acceptedAgo(t, e, model.AsyncGenerationDeadline+time.Minute)
	poll := &cancelPoller{pollStub: pollStub{result: nativeresult.PollResult{Status: "result_received", Output: json.RawMessage(`{"labels":[]}`)}}}
	_, err = e.RecoverOnce(ctx, "worker", now, func(context.Context, *model.NativeTask) (Poller, error) { return poll, nil }, nil)
	require.NoError(t, err)
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "completed", task.Status)
	require.Empty(t, poll.cancels)
	require.False(t, refundedWith(w, "platform_failure"))
}

// A received result whose delivery keeps failing is not a generation timeout;
// it keeps the long NativeTaskDeadline backstop.
func TestGenerationDeadlineSkipsReceivedResults(t *testing.T) {
	e, plan, w, _ := setup(t)
	ctx := context.Background()
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.NoError(t, model.SaveNativeTaskResult(e.DB, "req", "g", 1, []byte(`{"labels":[]}`)))
	now := acceptedAgo(t, e, time.Hour)
	w.fail = "execution_finished" // delivery cannot finish yet
	poll := &cancelPoller{}
	_, err = e.RecoverOnce(ctx, "worker", now, func(context.Context, *model.NativeTask) (Poller, error) { return poll, nil }, nil)
	require.NoError(t, err)
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "result_received", task.Status)
	require.Zero(t, poll.calls)
	require.Empty(t, poll.cancels)
}

// Younger tasks are untouched, and their next poll is pulled in to the
// deadline so the rule fires within seconds of 15 minutes, not ~2 later.
func TestGenerationDeadlineSchedulesAPollAtTheDeadline(t *testing.T) {
	cases := map[string]pollStub{
		"running": {result: nativeresult.PollResult{Status: "running"}},
		"error":   {err: errors.New("provider unreachable")},
	}
	for name, stub := range cases {
		t.Run(name, func(t *testing.T) {
			e, plan, w, _ := setup(t)
			ctx := context.Background()
			_, err := e.Submit(ctx, "req", "g", 1, body, plan)
			require.NoError(t, err)
			now := acceptedAgo(t, e, model.AsyncGenerationDeadline-30*time.Second)
			poll := &cancelPoller{pollStub: stub}
			_, err = e.RecoverOnce(ctx, "worker", now, func(context.Context, *model.NativeTask) (Poller, error) { return poll, nil }, nil)
			require.NoError(t, err)
			task, err := model.GetNativeTask(e.DB, "req", "g", 1)
			require.NoError(t, err)
			require.Contains(t, []string{"queued", "running"}, task.Status)
			require.False(t, refundedWith(w, "platform_failure"))
			require.Empty(t, poll.cancels)
			// Claimable only once the deadline has passed, and no later than ~2s after.
			require.Greater(t, task.NextRecoveryAt, now.Add(30*time.Second).Unix()-1)
			require.LessOrEqual(t, task.NextRecoveryAt, now.Add(32*time.Second).Unix())
			require.True(t, time.Unix(task.NextRecoveryAt, 0).After(task.CreatedAt.Add(model.AsyncGenerationDeadline)), "never claimed before the deadline, which would re-poll in a loop")
		})
	}
}

// Expiry is a compare-and-set: only the call that ended the task cancels, and
// a later pass only finishes the (idempotent) refund.
func TestGenerationDeadlineIsIdempotentAcrossWorkers(t *testing.T) {
	e, plan, w, _ := setup(t)
	ctx := context.Background()
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	now := acceptedAgo(t, e, model.AsyncGenerationDeadline+time.Second)
	expired, err := model.ExpireNativeGeneration(e.DB, "req", "g", 1, now)
	require.NoError(t, err)
	require.True(t, expired)
	again, err := model.ExpireNativeGeneration(e.DB, "req", "g", 1, now)
	require.NoError(t, err)
	require.False(t, again)
	poll := &cancelPoller{}
	_, err = e.RecoverOnce(ctx, "worker", now, func(context.Context, *model.NativeTask) (Poller, error) { return poll, nil }, nil)
	require.NoError(t, err)
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "failed", task.Status)
	require.Equal(t, model.AsyncGenerationTimeoutCode, task.ErrorCode)
	requireTimeoutRefund(t, w)
	require.Zero(t, poll.calls)
	require.Empty(t, poll.cancels)
}

// The deadline is enforced by the store itself, not only by the caller.
func TestExpireNativeGenerationRefusesTasksBeforeTheDeadline(t *testing.T) {
	e, plan, _, _ := setup(t)
	_, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	now := acceptedAgo(t, e, model.AsyncGenerationDeadline-time.Second)
	expired, err := model.ExpireNativeGeneration(e.DB, "req", "g", 1, now)
	require.NoError(t, err)
	require.False(t, expired)
	expired, err = model.ExpireNativeGeneration(e.DB, "req", "other", 1, now.Add(time.Hour))
	require.NoError(t, err)
	require.False(t, expired, "another group's task is never touched")
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "queued", task.Status)
}

// A saved result wins over a concurrent expiry and the reverse.
func TestGenerationDeadlineAndResultAreMutuallyExclusive(t *testing.T) {
	e, plan, _, _ := setup(t)
	_, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	now := acceptedAgo(t, e, time.Hour)
	require.NoError(t, model.SaveNativeTaskResult(e.DB, "req", "g", 1, []byte(`{"labels":[]}`)))
	expired, err := model.ExpireNativeGeneration(e.DB, "req", "g", 1, now)
	require.NoError(t, err)
	require.False(t, expired)

	e2, plan2, _, _ := setup(t)
	_, err = e2.Submit(context.Background(), "req", "g", 1, body, plan2)
	require.NoError(t, err)
	now = acceptedAgo(t, e2, time.Hour)
	expired, err = model.ExpireNativeGeneration(e2.DB, "req", "g", 1, now)
	require.NoError(t, err)
	require.True(t, expired)
	require.ErrorIs(t, model.SaveNativeTaskResult(e2.DB, "req", "g", 1, []byte(`{"labels":[]}`)), model.ErrNativeTaskConflict)
}

// A timed-out task that recovery left unsettled is refunded by any later sync.
func TestSyncBillingRefundsGenerationTimeout(t *testing.T) {
	e, plan, w, _ := setup(t)
	ctx := context.Background()
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "req").Updates(map[string]any{"status": "failed", "error_code": model.AsyncGenerationTimeoutCode}).Error)
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	w.commands = nil
	require.NoError(t, e.SyncBilling(ctx, task))
	require.Equal(t, []string{"accept_attempt", "settle:failed/platform_failure", "get"}, walletActions(w))
}

// Customers see a terminal failure with the owner-approved explanation.
func TestGenerationTimeoutPublicError(t *testing.T) {
	recorder := httptest.NewRecorder()
	WritePublic(recorder, http.StatusOK, &model.NativeTask{ID: "req", Model: "m", Status: "failed", ErrorCode: "generation_timeout"})
	var got struct {
		Status string `json:"status"`
		Error  struct{ Code, Message string }
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
	require.Equal(t, "failed", got.Status)
	require.Equal(t, "generation_timeout", got.Error.Code)
	require.Equal(t, "The provider did not finish this task within 15 minutes, so it was stopped. You were not charged; submit a new task with a new X-Request-Id.", got.Error.Message)
}

// The clock starts when the provider accepts the task, not when the request
// ID was first reserved (a resumed reservation can be hours old).
func TestGenerationClockStartsAtAcceptance(t *testing.T) {
	e, plan, _, _ := setup(t)
	_, _, err := model.ReserveNativeTask(e.DB, model.NativeTask{ID: "req", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "fp", OutputSchema: `{}`, FrozenContract: string(plan.Contract), BillingOperationID: "native:req"})
	require.NoError(t, err)
	acceptedAgo(t, e, 3*time.Hour)
	require.NoError(t, model.AcceptNativeTask(e.DB, "req", "g", 1, "upstream-1"))
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now(), task.CreatedAt, 5*time.Second)
	// Replaying the same acceptance does not move the clock again.
	acceptedAgo(t, e, 10*time.Minute)
	require.NoError(t, model.AcceptNativeTask(e.DB, "req", "g", 1, "upstream-1"))
	task, err = model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(-10*time.Minute), task.CreatedAt, 5*time.Second)
}
