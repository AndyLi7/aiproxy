package nativetask

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Own task queue review fixes (docs/handoffs/2026-10-15-own-queue-review-fixes.md).

// Review 1: a dispatched task is due for recovery at once, not at the end of
// the dispatch lease, so an accepted result is polled from the start.
func TestDispatchedTaskIsPolledAtOnce(t *testing.T) {
	env := newQueueEnv(t)
	task, err := env.submit("a1", "ga")
	require.NoError(t, err)
	require.Equal(t, "queued", task.Status, "sent on the fast path")
	require.Zero(t, env.task("a1", "ga").NextRecoveryAt)
	claimed, err := model.ClaimNativeRecovery(env.e.DB, "worker", time.Now(), 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, "a1", claimed[0].ID)
}

// Review 2: cancelling an unknown task never tells the customer to resend the POST.
func TestCancelOfAnUnknownTaskSaysThereIsNothingToCancel(t *testing.T) {
	env := newQueueEnv(t)
	response := httptest.NewRecorder()
	env.http().Cancel(response, customerRequest("POST", "/v1/model-tasks/missing/cancel", "", "ga"), "missing")
	require.Equal(t, 404, response.Code)
	require.JSONEq(t, `{"error":{"code":"task_not_found","message":"No task with this ID exists for this API key; there is nothing to cancel.","type":"not_found_error"}}`, response.Body.String())
	require.NotContains(t, response.Body.String(), "resend")
}

// Review 17: one wording for a task that can no longer be cancelled.
func TestNotCancellableWording(t *testing.T) {
	require.Contains(t, ErrorMessage("task_not_cancellable"), "no longer waiting in the queue (being sent, sent, or ended)")
}

// Review 3: a paused model holds back only that customer's tasks of it; their
// other tasks still go. The active limit still holds the whole customer back.
func TestPausedModelHoldsBackOnlyItsTasks(t *testing.T) {
	env := newQueueEnv(t)
	env.set(func() { env.limits = map[string]int{falScope: 3} })
	for _, id := range []string{"x1", "x2", "x3"} { // another customer fills the scope
		_, err := env.submit(id, "gx")
		require.NoError(t, err)
	}
	_, err := env.submitModel("paused1", "ga", "brand/model/paused")
	require.NoError(t, err)
	time.Sleep(2 * time.Millisecond)
	_, err = env.submit("other1", "ga")
	require.NoError(t, err)
	env.w.mu.Lock()
	env.w.pausedModels["brand/model/paused"] = true
	env.w.mu.Unlock()
	env.finish("x1")
	env.round() // paused1 is first in line: refused (423), held back
	require.Equal(t, model.NativeStatusWaiting, env.task("paused1", "ga").Status)
	require.False(t, queue.backedOff("ga"), "the customer is not held back")
	env.round() // the customer's next task takes the turn
	require.Equal(t, "queued", env.task("other1", "ga").Status)
	require.Equal(t, model.NativeStatusWaiting, env.task("paused1", "ga").Status)
	require.Equal(t, 1, countAction(env.w.actionsFor("paused1"), "begin_attempt"), "no second wallet call while held back")
}

// Review 4: the faster poll while tasks wait is for work in flight only; a
// finished task waiting for its bill keeps its normal delay.
func TestFasterPollOnlyForWorkInFlight(t *testing.T) {
	env := newQueueEnv(t)
	_, err := env.submit("x1", "gx")
	require.NoError(t, err)
	_, err = env.submit("a1", "ga")
	require.NoError(t, err)
	require.True(t, env.e.scopeHasWaiting(falScope))
	release := time.Now()
	age := 10 * time.Minute
	for status, ceiling := range map[string]bool{"queued": true, "running": true, "completed": false, "result_received": false} {
		fresh := &model.NativeTask{Status: status, QueueScope: falScope, UpstreamID: "u", CreatedAt: release.Add(-age)}
		next := env.e.recoveryNext(fresh, release, age, false, false, time.Time{})
		if ceiling {
			require.False(t, next.After(release.Add(queuedPollCeiling)), status)
		} else {
			require.Equal(t, release.Add(recoveryDelay(age, false)), next, status)
		}
	}
}

// Review 11: a fast path that sends the task does not wake the dispatcher
// first (it could take the task and the POST lose its answer); a fast path
// that cannot send wakes it.
func TestFastPathWakesTheDispatcherOnlyWhenItCannotSend(t *testing.T) {
	env := newQueueEnv(t)
	drain := func() bool {
		select {
		case <-queue.wake:
			return true
		default:
			return false
		}
	}
	drain()
	_, err := env.submit("a1", "ga")
	require.NoError(t, err)
	require.Equal(t, "queued", env.task("a1", "ga").Status)
	require.False(t, drain(), "sent on the fast path: no wake")
	_, err = env.submit("a2", "ga")
	require.NoError(t, err)
	require.Equal(t, model.NativeStatusWaiting, env.task("a2", "ga").Status)
	require.True(t, drain(), "queued: the dispatcher is woken")
}

// Review 13: the place counts from the wallet's admission, but the task may
// be claimed on the gateway's clock: a wallet clock ahead never stops the
// fast path.
func TestWalletClockAheadKeepsTheFastPath(t *testing.T) {
	env := newQueueEnv(t)
	env.w.clockSkew = 10 * time.Minute
	task, err := env.submit("a1", "ga")
	require.NoError(t, err)
	require.Equal(t, "queued", task.Status)
	stored := env.task("a1", "ga")
	require.True(t, stored.EnqueuedAt.After(time.Now().Add(5*time.Minute)), "the wallet's admission time is kept")
	require.False(t, stored.DispatchAfter.After(time.Now()))
}

// Review 14: shortening the queue timeout at run time also times out tasks
// already waiting (staging drill step 3).
func TestShortenedQueueTimeoutAppliesToWaitingTasks(t *testing.T) {
	env := newQueueEnv(t)
	_, err := env.submit("x1", "gx")
	require.NoError(t, err)
	_, err = env.submit("a1", "ga")
	require.NoError(t, err)
	require.NoError(t, env.e.DB.Model(&model.NativeTask{}).Where("id = ?", "a1").
		Update("enqueued_at", time.Now().Add(-2*time.Minute).UTC()).Error)
	env.set(func() { env.timeout = time.Minute })
	_, err = env.e.RecoverOnce(context.Background(), "worker", time.Now(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, model.NativeStatusWaiting, env.task("a1", "ga").Status, "its next look was set from 5 minutes")
	env.round()
	_, err = env.e.RecoverOnce(context.Background(), "worker", time.Now().Add(time.Second), nil, nil)
	require.NoError(t, err)
	task := env.task("a1", "ga")
	require.Equal(t, model.QueueTimeoutCode, task.ErrorCode)
	require.True(t, task.BillingSettled)
}

// Review 15: the wallet queued the hold but the enqueue failed on the
// database: answer 202 queued, never a retry hint; recovery enqueues it.
func TestEnqueueFailureAnswersQueuedAndRecoveryEnqueues(t *testing.T) {
	env := newQueueEnv(t)
	previous := enqueueNativeTask
	enqueueNativeTask = func(_ *gorm.DB, _ string, _ string, _ int, _ string, _, _ time.Time) error {
		return errors.New("database is unavailable")
	}
	t.Cleanup(func() { enqueueNativeTask = previous })
	response := httptest.NewRecorder()
	env.http().Create(response, customerRequest("POST", "/v1/model-tasks", "a1", "ga"))
	require.Equal(t, 202, response.Code, response.Body.String())
	require.JSONEq(t, `{"id":"a1","model":"brand/model/native","status":"queued"}`, response.Body.String())
	require.Equal(t, "reserved", env.task("a1", "ga").Status)
	enqueueNativeTask = previous
	_, err := env.e.RecoverOnce(context.Background(), "worker", time.Now().Add(reservedEnqueueGrace+time.Second), nil, nil)
	require.NoError(t, err)
	require.Equal(t, model.NativeStatusWaiting, env.task("a1", "ga").Status)
	env.round()
	require.Equal(t, "queued", env.task("a1", "ga").Status)
}

// Review 19: a task the cached estimate does not list is not read again
// within the cache, unless it entered the queue after the estimate.
func TestQueuePositionDoesNotRereadForUnlistedTasks(t *testing.T) {
	env := newQueueEnv(t)
	_, err := env.submit("x1", "gx")
	require.NoError(t, err)
	_, err = env.submit("a1", "ga")
	require.NoError(t, err)
	reads := 0
	previous := readQueueSnapshot
	readQueueSnapshot = func(db *gorm.DB, scope string, now time.Time, other model.OtherInFlight) (*model.NativeQueueSnapshot, error) {
		reads++
		return previous(db, scope, now, other)
	}
	t.Cleanup(func() { readQueueSnapshot = previous })
	require.Equal(t, 1, env.e.QueuePosition(env.task("a1", "ga")))
	require.Equal(t, 1, reads)
	old := &model.NativeTask{ID: "beyond-the-read-limit", Status: model.NativeStatusWaiting, QueueScope: falScope, UpdatedAt: time.Now().Add(-time.Minute)}
	for range 3 {
		require.Zero(t, env.e.QueuePosition(old))
	}
	require.Equal(t, 1, reads, "not read again within the cache")
	fresh := &model.NativeTask{ID: "just-queued", Status: model.NativeStatusWaiting, QueueScope: falScope, UpdatedAt: time.Now().Add(time.Second)}
	env.e.QueuePosition(fresh)
	require.Equal(t, 2, reads, "a task queued after the estimate is read")
}
