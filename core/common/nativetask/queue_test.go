package nativetask

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/stretchr/testify/require"
)

// Own task queue, spec 2026-10-15 §10.1. Unless a test changes it, fal's
// scope takes one task at a time and the queue is on.

func TestPublicStatusIsTheOnlyMapping(t *testing.T) {
	for status, want := range map[string]string{
		"reserved": "queued", model.NativeStatusWaiting: "queued", model.NativeStatusDispatching: "queued",
		"submitting": "queued", "submission_unknown": "queued", "queued": "queued",
		"running": "running", "result_received": "running", "delivery_ready": "running",
		"completed": "completed", "failed": "failed",
		// Never passed through: the application parses four statuses strictly.
		"something_new": "running", "": "running",
	} {
		require.Equal(t, want, PublicStatus(status), status)
	}
}

// §2 step 5: with nobody waiting and room, the request sends its own task and
// answers exactly as before the queue.
func TestFastPathAnswersAsBeforeWhenNobodyWaits(t *testing.T) {
	env := newQueueEnv(t)
	response := httptest.NewRecorder()
	env.http().Create(response, customerRequest("POST", "/v1/model-tasks", "a1", "ga"))
	require.Equal(t, 202, response.Code, response.Body.String())
	require.JSONEq(t, `{"id":"a1","model":"brand/model/native","status":"queued"}`, response.Body.String())
	require.Equal(t, 1, env.p.count())
	task := env.task("a1", "ga")
	require.Equal(t, "queued", task.Status)
	require.NotEmpty(t, task.UpstreamID)
	require.NotNil(t, task.EnqueuedAt)
	require.NotNil(t, task.DispatchedAt)
	require.Equal(t, falScope, task.QueueScope)
	require.Equal(t, []string{"admit:queue", "begin_attempt", "accept_attempt"}, env.w.actionsFor("a1"))

	// A provider rejection on the fast path answers failed and refunds, as before.
	env.set(func() { env.limits = map[string]int{} })
	env.p.err = &adaptor.ImageSubmissionFailure{Failure: failover.Failure{Acceptance: failover.NotAccepted}, ProviderStatus: 400}
	response = httptest.NewRecorder()
	env.http().Create(response, customerRequest("POST", "/v1/model-tasks", "a2", "ga"))
	require.Equal(t, 202, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"status":"failed"`)
	require.Equal(t, []string{"admit:queue", "begin_attempt", "reject_attempt", "settle:failed/not_accepted"}, env.w.actionsFor("a2"))
}

func TestFullScopeQueuesWithAPositionAndDispatchesWhenRoomFrees(t *testing.T) {
	env := newQueueEnv(t)
	_, err := env.submit("a1", "ga")
	require.NoError(t, err)
	response := httptest.NewRecorder()
	env.http().Create(response, customerRequest("POST", "/v1/model-tasks", "a2", "ga"))
	require.Equal(t, 202, response.Code)
	require.JSONEq(t, `{"id":"a2","model":"brand/model/native","status":"queued","queue_position":1}`, response.Body.String())
	require.Equal(t, model.NativeStatusWaiting, env.task("a2", "ga").Status)
	require.Equal(t, 1, env.p.count())
	require.Equal(t, []string{"admit:queue"}, env.w.actionsFor("a2"), "no claim while the scope is full")
	_, submission, _ := env.w.hold("a2")
	require.Equal(t, "waiting", submission)

	// A same-ID retry only reports the stored state: no claim, no wallet call (§6.11).
	retry := httptest.NewRecorder()
	env.http().Create(retry, customerRequest("POST", "/v1/model-tasks", "a2", "ga"))
	require.Equal(t, 202, retry.Code)
	require.Contains(t, retry.Body.String(), `"queue_position":1`)
	require.Equal(t, []string{"admit:queue"}, env.w.actionsFor("a2"))

	env.round()
	require.Equal(t, model.NativeStatusWaiting, env.task("a2", "ga").Status, "still full")
	env.finish("a1")
	env.round()
	require.Equal(t, "queued", env.task("a2", "ga").Status)
	require.Equal(t, 2, env.p.count())
	get := httptest.NewRecorder()
	env.http().Get(get, customerRequest("GET", "/v1/model-tasks/a2", "", "ga"), "a2")
	require.NotContains(t, get.Body.String(), "queue_position")
}

// §5.5, §5.6: a customer with one task passes one with many; within a
// customer, first in first out; the estimated positions are the real order.
func TestFairnessAndPositionsMatchTheDispatchOrder(t *testing.T) {
	env := newQueueEnv(t)
	env.set(func() { env.limits = map[string]int{falScope: 2} })
	for _, id := range []string{"x1", "x2"} { // another customer fills the scope
		_, err := env.submit(id, "gx")
		require.NoError(t, err)
	}
	groups := map[string]string{"x1": "gx", "x2": "gx", "b1": "gb"}
	for _, id := range []string{"a1", "a2", "a3", "a4"} {
		groups[id] = "ga"
		_, err := env.submit(id, "ga")
		require.NoError(t, err)
		time.Sleep(2 * time.Millisecond)
	}
	_, err := env.submit("b1", "gb")
	require.NoError(t, err)

	want := []string{"a1", "b1", "a2", "a3", "a4"}
	for place, id := range want {
		queue.positions = map[string]cachedPositions{}
		require.Equal(t, place+1, env.e.QueuePosition(env.task(id, groups[id])), id)
	}
	// Room for one more each round; nothing finishes, so the simulated order holds.
	order := []string{}
	for limit := 3; limit <= 7; limit++ {
		env.set(func() { env.limits = map[string]int{falScope: limit} })
		env.round()
		for _, id := range want {
			if env.task(id, groups[id]).Status == "queued" && !contains(order, id) {
				order = append(order, id)
			}
		}
		require.Len(t, order, limit-2)
	}
	require.Equal(t, want, order)
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// §6.3, §5.5 rule 0: a full active limit puts the task back unchanged and the
// customer is skipped without calling the wallet until a task of theirs ends.
func TestActiveLimitPutsTheTaskBackAndSkipsTheCustomer(t *testing.T) {
	env := newQueueEnv(t)
	env.set(func() { env.limits = map[string]int{falScope: 5} })
	env.w.activeLimit = 1
	_, err := env.submit("a1", "ga")
	require.NoError(t, err)
	require.Equal(t, "queued", env.task("a1", "ga").Status)
	queued, err := env.submit("a2", "ga")
	require.NoError(t, err)
	require.Equal(t, model.NativeStatusWaiting, queued.Status)
	stored := env.task("a2", "ga")
	require.Empty(t, stored.DispatchOwner)
	require.Nil(t, stored.DispatchedAt)
	require.Equal(t, []string{"admit:queue", "begin_attempt"}, env.w.actionsFor("a2"))
	status, submission, attempts := env.w.hold("a2")
	require.Equal(t, []any{"pending", "waiting", 0}, []any{status, submission, attempts})
	require.Equal(t, 1, env.p.count())
	require.Equal(t, 1, queue.activeLimit("ga"))

	// Backed off, then known to be at its limit: no further wallet call.
	queue.backoff = map[string]time.Time{}
	env.round()
	env.round()
	require.Equal(t, []string{"admit:queue", "begin_attempt"}, env.w.actionsFor("a2"))
	queue.mu.Lock()
	queue.positions = map[string]cachedPositions{}
	queue.mu.Unlock()
	require.Equal(t, 1, env.e.QueuePosition(stored), "its place is kept")
	require.Equal(t, stored.EnqueuedAt.UTC(), env.task("a2", "ga").EnqueuedAt.UTC())

	env.finish("a1")
	env.round()
	require.Equal(t, "queued", env.task("a2", "ga").Status)
	require.Equal(t, 2, env.p.count())
}

func TestPausedModelPutsTheTaskBack(t *testing.T) {
	env := newQueueEnv(t)
	_, err := env.submit("x1", "gx")
	require.NoError(t, err)
	_, err = env.submit("a1", "ga")
	require.NoError(t, err)
	env.w.mu.Lock()
	env.w.paused = true
	env.w.mu.Unlock()
	env.finish("x1")
	env.round()
	require.Equal(t, model.NativeStatusWaiting, env.task("a1", "ga").Status)
	_, submission, attempts := env.w.hold("a1")
	require.Equal(t, "waiting", submission)
	require.Zero(t, attempts)
	require.Equal(t, 1, env.p.count())
	// Review 3: only this customer's tasks of this model are held back.
	require.False(t, queue.backedOff("ga"))
	require.True(t, queue.modelBackedOff(model.NativeQueueEntry{GroupID: "ga", Model: "brand/model/native"}))

	env.w.mu.Lock()
	env.w.paused = false
	env.w.mu.Unlock()
	queue.modelBackoff = map[string]time.Time{}
	env.round()
	require.Equal(t, "queued", env.task("a1", "ga").Status)
}

// §6.2: cancelling a waiting task refunds it without a provider call; a task
// claimed first cannot be cancelled. Cancel is idempotent and owner-scoped.
func TestCancelAndDispatchExcludeEachOther(t *testing.T) {
	t.Run("cancel first", func(t *testing.T) {
		env := newQueueEnv(t)
		_, err := env.submit("x1", "gx")
		require.NoError(t, err)
		_, err = env.submit("a1", "ga")
		require.NoError(t, err)
		h := env.http()
		for range 2 {
			response := httptest.NewRecorder()
			h.Cancel(response, customerRequest("POST", "/v1/model-tasks/a1/cancel", "", "ga"), "a1")
			require.Equal(t, 200, response.Code, response.Body.String())
			var public publicTask
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &public))
			require.Equal(t, "failed", public.Status)
			require.Equal(t, model.CancelledCode, public.Error.Code)
		}
		require.Equal(t, []string{"admit:queue", "get", "settle:failed/not_dispatched", "get"}, env.w.actionsFor("a1"))
		status, _, _ := env.w.hold("a1")
		require.Equal(t, "refunded", status)
		require.True(t, env.task("a1", "ga").BillingSettled)
		env.finish("x1")
		env.round()
		require.Equal(t, 1, env.p.count(), "never sent")

		other := httptest.NewRecorder()
		h.Cancel(other, customerRequest("POST", "/v1/model-tasks/a1/cancel", "", "gb"), "a1")
		require.Equal(t, 404, other.Code)
	})
	t.Run("claim first", func(t *testing.T) {
		env := newQueueEnv(t)
		_, err := env.submit("x1", "gx")
		require.NoError(t, err)
		_, err = env.submit("a1", "ga")
		require.NoError(t, err)
		env.finish("x1")
		claimed, _, err := env.e.claim(falScope, "")
		require.NoError(t, err)
		require.Equal(t, "a1", claimed.ID)
		response := httptest.NewRecorder()
		env.http().Cancel(response, customerRequest("POST", "/v1/model-tasks/a1/cancel", "", "ga"), "a1")
		require.Equal(t, 409, response.Code)
		require.JSONEq(t, `{"error":{"code":"task_not_cancellable","message":"`+ErrorMessage("task_not_cancellable")+`","type":"invalid_request_error"},"status":"queued"}`, response.Body.String())
		_, err = env.e.dispatchClaimed(context.Background(), claimed, nil)
		require.NoError(t, err)
		require.Equal(t, "queued", env.task("a1", "ga").Status)
		require.Equal(t, 2, env.p.count())
	})
	t.Run("concurrent", func(t *testing.T) {
		for range 10 {
			env := newQueueEnv(t)
			_, err := env.submit("x1", "gx")
			require.NoError(t, err)
			_, err = env.submit("a1", "ga")
			require.NoError(t, err)
			env.finish("x1")
			response := httptest.NewRecorder()
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); env.round() }()
			go func() {
				defer wg.Done()
				env.http().Cancel(response, customerRequest("POST", "/v1/model-tasks/a1/cancel", "", "ga"), "a1")
			}()
			wg.Wait()
			task := env.task("a1", "ga")
			status, _, _ := env.w.hold("a1")
			if response.Code == 200 {
				require.Equal(t, model.CancelledCode, task.ErrorCode)
				require.Equal(t, 1, env.p.count())
				require.Equal(t, "refunded", status)
			} else {
				require.Equal(t, 409, response.Code)
				require.Equal(t, "queued", task.Status)
				require.Equal(t, 2, env.p.count())
				require.Equal(t, "pending", status)
			}
		}
	})
	t.Run("reserved", func(t *testing.T) {
		env := newQueueEnv(t)
		env.w.lose["admit"] = true
		_, err := env.submit("a1", "ga")
		require.Error(t, err)
		require.Equal(t, "reserved", env.task("a1", "ga").Status)
		response := httptest.NewRecorder()
		env.http().Cancel(response, customerRequest("POST", "/v1/model-tasks/a1/cancel", "", "ga"), "a1")
		require.Equal(t, 409, response.Code)
	})
}

// §6.1: past its queue timeout a waiting task is failed and refunded; a task
// claimed in time is not timed out.
func TestQueueTimeoutAndDispatchExcludeEachOther(t *testing.T) {
	t.Run("timeout first", func(t *testing.T) {
		env := newQueueEnv(t)
		_, err := env.submit("x1", "gx")
		require.NoError(t, err)
		_, err = env.submit("a1", "ga")
		require.NoError(t, err)
		_, err = env.e.RecoverOnce(context.Background(), "worker", time.Now().Add(5*time.Minute+2*time.Second), nil, nil)
		require.NoError(t, err)
		task := env.task("a1", "ga")
		require.Equal(t, "failed", task.Status)
		require.Equal(t, model.QueueTimeoutCode, task.ErrorCode)
		require.True(t, task.BillingSettled)
		require.Equal(t, []string{"admit:queue", "get", "settle:failed/not_dispatched", "get"}, env.w.actionsFor("a1"))
		env.finish("x1")
		env.round()
		require.Equal(t, 1, env.p.count())
		get := httptest.NewRecorder()
		env.http().Get(get, customerRequest("GET", "/v1/model-tasks/a1", "", "ga"), "a1")
		require.Contains(t, get.Body.String(), "waited in the queue too long")
	})
	t.Run("claim first", func(t *testing.T) {
		env := newQueueEnv(t)
		env.set(func() { env.timeout = time.Second })
		_, err := env.submit("x1", "gx")
		require.NoError(t, err)
		_, err = env.submit("a1", "ga")
		require.NoError(t, err)
		env.finish("x1")
		claimed, _, err := env.e.claim(falScope, "")
		require.NoError(t, err)
		_, err = env.e.RecoverOnce(context.Background(), "worker", time.Now().Add(3*time.Second), nil, nil)
		require.NoError(t, err)
		require.Equal(t, model.NativeStatusDispatching, env.task("a1", "ga").Status)
		_, err = env.e.dispatchClaimed(context.Background(), claimed, nil)
		require.NoError(t, err)
		require.Equal(t, "queued", env.task("a1", "ga").Status)
	})
}

// §6.6: a claim whose lease ran out always fails as not_started and is
// refunded by what the wallet holds; it is never put back in the queue.
func TestExpiredDispatchLeaseFailsAndRefunds(t *testing.T) {
	claimOnly := func(t *testing.T) (*queueEnv, *model.NativeTask) {
		env := newQueueEnv(t)
		_, err := env.submit("x1", "gx")
		require.NoError(t, err)
		_, err = env.submit("a1", "ga")
		require.NoError(t, err)
		env.finish("x1")
		claimed, _, err := env.e.claim(falScope, "")
		require.NoError(t, err)
		require.NotNil(t, claimed)
		return env, claimed
	}
	t.Run("no attempt: not_dispatched", func(t *testing.T) {
		env, _ := claimOnly(t)
		_, err := env.e.RecoverOnce(context.Background(), "worker", time.Now().Add(time.Minute), nil, nil)
		require.NoError(t, err)
		require.Equal(t, model.NativeStatusDispatching, env.task("a1", "ga").Status, "lease still held")
		_, err = env.e.RecoverOnce(context.Background(), "worker", time.Now().Add(model.NativeDispatchLease+2*time.Second), nil, nil)
		require.NoError(t, err)
		task := env.task("a1", "ga")
		require.Equal(t, "failed", task.Status)
		require.Equal(t, model.NotStartedCode, task.ErrorCode)
		require.Equal(t, []string{"admit:queue", "get", "settle:failed/not_dispatched", "get"}, env.w.actionsFor("a1"))
		require.Equal(t, 1, env.p.count())
	})
	t.Run("claimed attempt: not started refund", func(t *testing.T) {
		env, _ := claimOnly(t)
		_, err := env.w.Prepayment(context.Background(), balance.PrepaymentCommand{Action: "begin_attempt", Group: "ga", BillingOperationID: "native:a1", RouteID: "r", AttemptID: "native:a1:1"})
		require.NoError(t, err)
		_, err = env.e.RecoverOnce(context.Background(), "worker", time.Now().Add(model.NativeDispatchLease+2*time.Second), nil, nil)
		require.NoError(t, err)
		require.Equal(t, model.NotStartedCode, env.task("a1", "ga").ErrorCode)
		require.Equal(t, []string{"admit:queue", "begin_attempt", "get", "reject_attempt", "settle:failed/not_accepted", "get"}, env.w.actionsFor("a1"))
		status, submission, _ := env.w.hold("a1")
		require.Equal(t, "refunded", status)
		require.Equal(t, "not_submitted", submission, "the wallet is not left unknown")
	})
	t.Run("a stale holder changes nothing", func(t *testing.T) {
		env, claimed := claimOnly(t)
		db := env.e.DB
		require.ErrorIs(t, model.ReturnNativeDispatch(db, "a1", "ga", 1, "stale", time.Now()), model.ErrNativeTaskConflict)
		require.ErrorIs(t, model.FailNativeDispatch(db, "a1", "ga", 1, "stale", model.NotStartedCode), model.ErrNativeTaskConflict)
		require.ErrorIs(t, model.StartNativeDispatchSubmission(db, "a1", "ga", 1, "stale", time.Now()), model.ErrNativeTaskConflict)
		expired, err := model.ExpireNativeDispatch(db, "a1", "ga", 1, "stale", time.Now().Add(time.Hour))
		require.NoError(t, err)
		require.False(t, expired)
		require.Equal(t, claimed.DispatchOwner, env.task("a1", "ga").DispatchOwner)
		require.Equal(t, model.NativeStatusDispatching, env.task("a1", "ga").Status)
	})
}

// Review H1 (§6.6): the claim reaches the wallet slowly, the lease runs out
// and recovery ends the task before the claim's reply arrives. The task
// fails and is refunded in full; the late claimed:true never sends it; the
// wallet is never left with an unknown attempt.
func TestSlowClaimAfterTheLeaseNeverSends(t *testing.T) {
	for _, landed := range []string{"before recovery", "after recovery"} {
		t.Run("claim landed "+landed, func(t *testing.T) {
			env := newQueueEnv(t)
			_, err := env.submit("x1", "gx")
			require.NoError(t, err)
			_, err = env.submit("a1", "ga")
			require.NoError(t, err)
			env.finish("x1")
			claimed, _, err := env.e.claim(falScope, "")
			require.NoError(t, err)
			expire := func(balance.PrepaymentCommand) {
				_, recoverErr := env.e.RecoverOnce(context.Background(), "worker", time.Now().Add(model.NativeDispatchLease+2*time.Second), nil, nil)
				require.NoError(t, recoverErr)
				// Real time has not reached the lease: let the holder see it run out.
				require.NoError(t, env.e.DB.Model(&model.NativeTask{}).Where("id = ?", "a1").Update("dispatch_until", time.Now().Add(-time.Second).UTC()).Error)
			}
			if landed == "before recovery" {
				env.w.afterBegin = expire
			} else {
				env.w.beforeBegin = expire
			}
			_, _ = env.e.dispatchClaimed(context.Background(), claimed, nil)
			task := env.task("a1", "ga")
			require.Equal(t, "failed", task.Status)
			require.Equal(t, model.NotStartedCode, task.ErrorCode)
			require.Equal(t, 1, env.p.count(), "never sent")
			status, submission, _ := env.w.hold("a1")
			require.Equal(t, "refunded", status)
			require.NotEqual(t, "unknown", submission)
		})
	}
}

// §4.2: a lost claim reply (claimed:false on the retry) is never sent and
// never put back in the queue.
func TestClaimedFalseFailsAndRefunds(t *testing.T) {
	env := newQueueEnv(t)
	_, err := env.submit("x1", "gx")
	require.NoError(t, err)
	_, err = env.submit("a1", "ga")
	require.NoError(t, err)
	env.finish("x1")
	_, err = env.w.Prepayment(context.Background(), balance.PrepaymentCommand{Action: "begin_attempt", Group: "ga", BillingOperationID: "native:a1", RouteID: "r", AttemptID: "native:a1:1"})
	require.NoError(t, err)
	env.round()
	task := env.task("a1", "ga")
	require.Equal(t, "failed", task.Status)
	require.Equal(t, model.NotStartedCode, task.ErrorCode)
	require.Equal(t, 1, env.p.count())
	status, _, _ := env.w.hold("a1")
	require.Equal(t, "refunded", status)
}

// §4.7: a balance below zero when the task's turn comes fails it and refunds
// it, without a provider call.
func TestNegativeBalanceAtDispatchFailsAndRefunds(t *testing.T) {
	env := newQueueEnv(t)
	_, err := env.submit("x1", "gx")
	require.NoError(t, err)
	_, err = env.submit("a1", "ga")
	require.NoError(t, err)
	env.w.mu.Lock()
	env.w.balances["ga"] = -1
	env.w.mu.Unlock()
	env.finish("x1")
	env.round()
	task := env.task("a1", "ga")
	require.Equal(t, "failed", task.Status)
	require.Equal(t, model.InsufficientBalanceCode, task.ErrorCode)
	require.Equal(t, []string{"admit:queue", "begin_attempt", "get", "settle:failed/not_dispatched", "get"}, env.w.actionsFor("a1"))
	require.Equal(t, 1, env.p.count())
}

// §6.4: a frozen channel no longer usable fails the task, refunded as not dispatched.
func TestUnusableFrozenChannelFailsTheQueuedTask(t *testing.T) {
	env := newQueueEnv(t)
	_, err := env.submit("x1", "gx")
	require.NoError(t, err)
	_, err = env.submit("a1", "ga")
	require.NoError(t, err)
	env.set(func() { env.unusable = true })
	env.finish("x1")
	env.round()
	task := env.task("a1", "ga")
	require.Equal(t, model.ModelUnavailableWhileQueuedCode, task.ErrorCode)
	require.Equal(t, []string{"admit:queue", "get", "settle:failed/not_dispatched", "get"}, env.w.actionsFor("a1"))
	require.Equal(t, 1, env.p.count())
}

// §4.3: a refund that fails right after the cancel is retried by recovery.
func TestFailedCancelRefundIsRetriedByRecovery(t *testing.T) {
	env := newQueueEnv(t)
	_, err := env.submit("x1", "gx")
	require.NoError(t, err)
	_, err = env.submit("a1", "ga")
	require.NoError(t, err)
	env.w.down["settle"] = true
	response := httptest.NewRecorder()
	env.http().Cancel(response, customerRequest("POST", "/v1/model-tasks/a1/cancel", "", "ga"), "a1")
	require.Equal(t, 200, response.Code)
	require.False(t, env.task("a1", "ga").BillingSettled)
	env.w.down = map[string]bool{}
	_, err = env.e.RecoverOnce(context.Background(), "worker", time.Now().Add(time.Second), nil, nil)
	require.NoError(t, err)
	require.True(t, env.task("a1", "ga").BillingSettled)
	status, _, _ := env.w.hold("a1")
	require.Equal(t, "refunded", status)
}

// §2 step 4: the wallet's answer picks the path, not the switch, when the
// switch changed between a hold's admission and its retry.
func TestSwitchChangeFollowsTheWalletHold(t *testing.T) {
	t.Run("queued hold, switch now off", func(t *testing.T) {
		env := newQueueEnv(t)
		env.w.lose["admit"] = true
		_, err := env.submit("a1", "ga")
		require.Error(t, err)
		env.set(func() { env.enabled = false })
		task, err := env.submit("a1", "ga")
		require.NoError(t, err)
		require.Equal(t, "queued", task.Status)
		require.NotNil(t, env.task("a1", "ga").EnqueuedAt, "went through the queue")
		require.Equal(t, []string{"admit:queue", "admit", "begin_attempt", "accept_attempt"}, env.w.actionsFor("a1"))
	})
	t.Run("direct hold, switch now on", func(t *testing.T) {
		env := newQueueEnv(t)
		env.set(func() { env.enabled = false })
		env.w.lose["admit"] = true
		_, err := env.submit("a1", "ga")
		require.Error(t, err)
		env.set(func() { env.enabled = true })
		task, err := env.submit("a1", "ga")
		require.NoError(t, err)
		require.Equal(t, "queued", task.Status)
		require.Nil(t, env.task("a1", "ga").EnqueuedAt, "went direct")
		_, submission, _ := env.w.hold("a1")
		require.Equal(t, "accepted", submission)
	})
}

// §6.10: releasing a waiting or dispatching task keeps its queue timeout or
// lease as recovery's next look, including when recovery held the row while
// the request enqueued it (a new row has next_recovery_at 0).
func TestRecoveryKeepsQueueTimesWhenReleasing(t *testing.T) {
	env := newQueueEnv(t)
	_, err := env.submit("x1", "gx")
	require.NoError(t, err)
	_, err = env.submit("a1", "ga")
	require.NoError(t, err)
	waiting := env.task("a1", "ga")
	due := waiting.EnqueuedAt.Add(5 * time.Minute).Add(time.Second).Unix()
	require.Equal(t, due, waiting.NextRecoveryAt)
	_, err = env.e.RecoverOnce(context.Background(), "worker", time.Now().Add(6*time.Minute), nil, nil)
	require.NoError(t, err)

	env2 := newQueueEnv(t)
	_, err = env2.submit("x1", "gx")
	require.NoError(t, err)
	_, err = env2.submit("a1", "ga")
	require.NoError(t, err)
	env2.finish("x1")
	claimed, _, err := env2.e.claim(falScope, "")
	require.NoError(t, err)
	require.Equal(t, claimed.DispatchUntil.Unix(), env2.task("a1", "ga").NextRecoveryAt)

	// Recovery leases a reserved row; the request enqueues it meanwhile.
	env3 := newQueueEnv(t)
	reserved, _, err := model.ReserveNativeTask(env3.e.DB, model.NativeTask{ID: "r1", GroupID: "ga", TokenID: 1, Model: "brand/model/native", Fingerprint: "f", OutputSchema: `{"type":"object"}`, BillingOperationID: "native:r1", QueueScope: falScope})
	require.NoError(t, err)
	leased, err := model.ClaimNativeRecovery(env3.e.DB, "worker", time.Now(), 1)
	require.NoError(t, err)
	require.NotEmpty(t, leased)
	enqueuedAt := time.Now()
	require.NoError(t, model.EnqueueNativeTask(env3.e.DB, "r1", "ga", 1, falScope, enqueuedAt, enqueuedAt.Add(5*time.Minute+time.Second)))
	require.NoError(t, env3.e.releaseRecovery("r1", "ga", 1, "worker", reserved, time.Now(), 0, false, false, time.Time{}))
	require.Equal(t, enqueuedAt.Add(5*time.Minute+time.Second).Unix(), env3.task("r1", "ga").NextRecoveryAt)
}

// §6.7: the process stopped after the queued admission and before the
// enqueue. Recovery completes the enqueue; the stop-loss never touches it.
func TestRecoveryEnqueuesAReservedTaskWithAWaitingHold(t *testing.T) {
	env := newQueueEnv(t)
	_, err := env.submit("x1", "gx")
	require.NoError(t, err)
	env.w.lose["admit"] = true
	_, err = env.submit("a1", "ga")
	require.Error(t, err)
	require.Equal(t, "reserved", env.task("a1", "ga").Status)
	_, err = env.e.RecoverOnce(context.Background(), "worker", time.Now(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, "reserved", env.task("a1", "ga").Status, "a live request may still enqueue it")
	_, err = env.e.RecoverOnce(context.Background(), "worker", time.Now().Add(reservedEnqueueGrace+time.Second), nil, nil)
	require.NoError(t, err)
	task := env.task("a1", "ga")
	require.Equal(t, model.NativeStatusWaiting, task.Status)
	require.NotNil(t, task.EnqueuedAt)
	for _, action := range env.w.actionsFor("a1") {
		require.NotContains(t, action, "settle")
	}
	// Long after the stop-loss grace it is timed out by the queue, never abandoned.
	_, err = env.e.RecoverOnce(context.Background(), "worker", time.Now().Add(reservationGrace+time.Minute), nil, nil)
	require.NoError(t, err)
	require.Equal(t, model.QueueTimeoutCode, env.task("a1", "ga").ErrorCode)
}

// §5.8: polling a task whose stored receipt shows its attempt accepted sends
// no accept_attempt, so faster polling adds no serialized wallet write.
func TestAcceptedAttemptIsNotAcceptedAgain(t *testing.T) {
	env := newQueueEnv(t)
	task, err := env.submit("a1", "ga")
	require.NoError(t, err)
	require.NoError(t, env.e.SyncBilling(context.Background(), env.task("a1", "ga")))
	require.Equal(t, 2, env.w.count("accept_attempt"), "the first sync stores a receipt without the acceptance")
	require.NoError(t, env.e.SyncBilling(context.Background(), env.task("a1", "ga")))
	require.NoError(t, env.e.SyncBilling(context.Background(), env.task("a1", "ga")))
	require.Equal(t, 2, env.w.count("accept_attempt"))
	require.Equal(t, "queued", task.Status)
}

// §2: with the switch off new tasks go straight to the provider, and tasks
// already queued are still dispatched.
func TestSwitchOffSendsDirectlyAndDrainsTheQueue(t *testing.T) {
	env := newQueueEnv(t)
	_, err := env.submit("x1", "gx")
	require.NoError(t, err)
	_, err = env.submit("a1", "ga")
	require.NoError(t, err)
	env.set(func() { env.enabled = false })
	task, err := env.submit("b1", "gb")
	require.NoError(t, err)
	require.Equal(t, "queued", task.Status)
	require.Nil(t, env.task("b1", "gb").EnqueuedAt)
	require.Equal(t, []string{"admit", "begin_attempt", "accept_attempt"}, env.w.actionsFor("b1"))
	require.Equal(t, falScope, env.task("b1", "gb").QueueScope, "direct tasks still count in flight")
	env.finish("x1")
	env.finish("b1")
	env.round()
	require.Equal(t, "queued", env.task("a1", "ga").Status)
}

// §5.3: image tasks of the provider and direct (trial) tasks count in flight.
func TestInFlightCountsImageTasksAndTrials(t *testing.T) {
	env := newQueueEnv(t)
	require.NoError(t, env.e.DB.Create(&model.ImageTask{ID: "img", GroupID: "gi", TokenID: 1, Status: "in_progress", ChannelType: model.ChannelTypeFal}).Error)
	require.NoError(t, env.e.DB.Create(&model.ImageTask{ID: "other", GroupID: "gi", TokenID: 1, Status: "in_progress", ChannelType: model.ChannelTypeFal + 1}).Error)
	task, err := env.submit("a1", "ga")
	require.NoError(t, err)
	require.Equal(t, model.NativeStatusWaiting, task.Status, "the fal image task fills the scope")
	require.NoError(t, env.e.DB.Model(&model.ImageTask{}).Where("id = ?", "img").Update("status", "completed").Error)

	trial := env.plan
	trial.Direct = true
	engine := *env.e
	engine.Provider = env.p
	sent, err := engine.Submit(context.Background(), "trial_1", "internal", 1, body, trial)
	require.NoError(t, err)
	require.Equal(t, "queued", sent.Status, "trials are never queued")
	require.Equal(t, []string{"admit", "begin_attempt", "accept_attempt"}, env.w.actionsFor("trial_1"))
	env.round()
	require.Equal(t, model.NativeStatusWaiting, env.task("a1", "ga").Status, "the trial fills the scope")
	env.finish("trial_1")
	env.round()
	require.Equal(t, "queued", env.task("a1", "ga").Status)
}

// §4.1: a full queue refuses with 429 too_many_active_tasks and a queue message.
func TestFullQueueRefusesWithAQueueMessage(t *testing.T) {
	env := newQueueEnv(t)
	env.w.maxWaiting = 1
	_, err := env.submit("x1", "gx")
	require.NoError(t, err)
	_, err = env.submit("a1", "ga")
	require.NoError(t, err)
	response := httptest.NewRecorder()
	env.http().Create(response, customerRequest("POST", "/v1/model-tasks", "a2", "ga"))
	require.Equal(t, 429, response.Code)
	require.Contains(t, response.Body.String(), `"code":"too_many_active_tasks"`)
	require.Contains(t, response.Body.String(), queueFullMessage)
	_, err = model.GetNativeTask(env.e.DB, "a2", "ga", 1)
	require.Error(t, err, "a refused request leaves no row")
}

// §6.8: on shutdown the dispatcher stops claiming and returns only after the
// tasks it claimed were sent.
func TestDispatcherWaitsForClaimedTasksOnShutdown(t *testing.T) {
	env := newQueueEnv(t)
	_, err := env.submit("x1", "gx")
	require.NoError(t, err)
	_, err = env.submit("a1", "ga")
	require.NoError(t, err)
	env.finish("x1")
	entered, release := make(chan struct{}), make(chan struct{})
	env.w.beforeBegin = func(balance.PrepaymentCommand) {
		close(entered)
		<-release
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunDispatcher(ctx, func() (*Engine, bool) { return env.e, true })
		close(done)
	}()
	wakeDispatcher()
	<-entered
	cancel()
	select {
	case <-done:
		t.Fatal("the dispatcher returned while a claimed task was being sent")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-done
	require.Equal(t, "queued", env.task("a1", "ga").Status)
}

func TestFalDispatchResolverRefusesAChangedChannel(t *testing.T) {
	key := "test-native-key"
	task := &model.NativeTask{ChannelID: 1, KeyFingerprint: model.ImageChannelKeyFingerprint(key)}
	for name, tc := range map[string]struct {
		channel *model.Channel
		err     error
		want    error
	}{
		"enabled":      {channel: &model.Channel{ID: 1, Type: model.ChannelTypeFal, Key: key, Status: model.ChannelStatusEnabled}},
		"disabled":     {channel: &model.Channel{ID: 1, Type: model.ChannelTypeFal, Key: key, Status: model.ChannelStatusDisabled}, want: ErrChannelUnusable},
		"rekeyed":      {channel: &model.Channel{ID: 1, Type: model.ChannelTypeFal, Key: "other", Status: model.ChannelStatusEnabled}, want: ErrChannelUnusable},
		"retargeted":   {channel: &model.Channel{ID: 1, Type: model.ChannelTypeFal, Key: key, Status: model.ChannelStatusEnabled, BaseURL: "https://other.example"}, want: ErrChannelUnusable},
		"deleted":      {err: model.NotFoundError(model.ErrChannelNotFound), want: ErrChannelUnusable},
		"read failed":  {err: context.DeadlineExceeded, want: ErrUnavailable},
		"another type": {channel: &model.Channel{ID: 1, Type: model.ChannelTypeFal + 1, Key: key, Status: model.ChannelStatusEnabled}, want: ErrChannelUnusable},
	} {
		t.Run(name, func(t *testing.T) {
			provider, err := ResolveFalDispatch(func(int) (*model.Channel, error) { return tc.channel, tc.err })(context.Background(), task)
			if tc.want == nil {
				require.NoError(t, err)
				require.NotNil(t, provider)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
		})
	}
}

// SQLite (single writer) serializes claims: with room for one, one wins.
func TestConcurrentClaimsNeverExceedTheLimit(t *testing.T) {
	env := newQueueEnv(t)
	_, err := env.submit("x1", "gx")
	require.NoError(t, err)
	for _, id := range []string{"a1", "a2", "a3", "a4"} {
		_, err = env.submit(id, "g"+id)
		require.NoError(t, err)
	}
	env.finish("x1")
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := 0
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task, _, claimErr := env.e.claim(falScope, "")
			require.NoError(t, claimErr)
			if task != nil {
				mu.Lock()
				claimed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, claimed)
}

// §6.11: a same-ID retry of a claimed (dispatching) task only reports it.
func TestRetryOfAClaimedTaskNeverDispatchesIt(t *testing.T) {
	env := newQueueEnv(t)
	_, err := env.submit("x1", "gx")
	require.NoError(t, err)
	_, err = env.submit("a1", "ga")
	require.NoError(t, err)
	env.finish("x1")
	claimed, _, err := env.e.claim(falScope, "")
	require.NoError(t, err)
	require.Equal(t, "a1", claimed.ID)
	before := env.w.actionsFor("a1")
	retry := httptest.NewRecorder()
	env.http().Create(retry, customerRequest("POST", "/v1/model-tasks", "a1", "ga"))
	require.Equal(t, 202, retry.Code)
	require.JSONEq(t, `{"id":"a1","model":"brand/model/native","status":"queued"}`, retry.Body.String())
	require.Equal(t, before, env.w.actionsFor("a1"))
	require.Equal(t, 1, env.p.count())
	require.Equal(t, model.NativeStatusDispatching, env.task("a1", "ga").Status)
}
