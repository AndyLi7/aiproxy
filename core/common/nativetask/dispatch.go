package nativetask

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
)

// dispatchRound bounds one dispatcher round's wait between rounds (spec §5.7).
const dispatchRoundInterval = time.Second

// enqueue puts a reserved task whose wallet hold is waiting into the queue
// (reserved -> waiting), writes its call row (202, pending), then tries the
// fast path: with no other task waiting in the scope and room in it, the
// request claims its own task and sends it now, so an empty queue answers
// exactly as before (spec §2 steps 4-6).
func (e *Engine) enqueue(ctx context.Context, task *model.NativeTask, logInfo *model.NativeTaskLog, admitted balance.PrepaymentReceipt) (*model.NativeTask, error) {
	enqueuedAt := queue.now()
	if at, ok := admitted.AdmittedAt(); ok {
		// A retry of a reserved task keeps its first admission's place.
		enqueuedAt = at
	}
	scope := task.QueueScope
	if scope == "" {
		route, routeErr := dispatchRoute(task)
		if routeErr != nil {
			return e.storedUnlessReserved(task, routeErr)
		}
		scope = QueueScope(route.Provider)
	}
	wake := enqueuedAt.Add(e.queueTimeout()).Add(time.Second)
	if err := model.EnqueueNativeTask(e.DB, task.ID, task.GroupID, task.TokenID, scope, enqueuedAt, wake); err != nil {
		// Recovery may have enqueued it first (spec §6.7): answer that state.
		return e.storedUnlessReserved(task, err)
	}
	task.Status = model.NativeStatusWaiting
	task.QueueScope = scope
	task.EnqueuedAt = &enqueuedAt
	if logInfo != nil {
		// The customer is answered 202 now, so the call row is written now.
		if logErr := model.RecordNativeTaskLog(e.DB, task, *logInfo); logErr != nil {
			log.Errorf("record native task log %s: %v", task.ID, logErr)
		}
	}
	wakeDispatcher()
	claimed, _, err := e.claim(scope, task.ID)
	if err != nil {
		log.WithFields(log.Fields{"lane": "native", "task_id": task.ID, "group": task.GroupID, "scope": scope}).
			WithError(err).Warn("native queue fast path unavailable; the dispatcher sends the task")
	}
	if claimed == nil {
		return e.storedQueued(task), nil
	}
	return e.dispatchClaimed(ctx, claimed, e.Provider)
}

// storedQueued is the stored task, or task when it cannot be read.
func (e *Engine) storedQueued(task *model.NativeTask) *model.NativeTask {
	if stored, err := model.GetNativeTask(e.DB, task.ID, task.GroupID, task.TokenID); err == nil {
		return stored
	}
	return task
}

// dispatchRoute selects the route a queued task is sent on. v1 returns the
// route frozen at admission: a model has one channel. A later version may
// choose among the quote's routes here, by room in their scopes (spec §11.4).
func dispatchRoute(task *model.NativeTask) (*model.ImagePrepaymentRoute, error) {
	quote, err := model.ParseImagePrepaymentQuote(task.PrepaymentQuoteJSON)
	if err != nil {
		return nil, err
	}
	route := quote.Route(task.ChannelID, task.Endpoint)
	if route == nil {
		return nil, ErrUnavailable
	}
	return route, nil
}

// dispatchClaimed sends a task this process claimed from the queue (spec §2,
// dispatch steps 2-4). provider is the submitting request's client on the
// fast path, else nil: the provider's QueueProvider resolves the frozen
// channel. Every move out of dispatching carries the claim's token. A wallet
// refusal that leaves the hold unchanged puts the task back in the queue;
// anything that may have claimed the attempt never does.
func (e *Engine) dispatchClaimed(ctx context.Context, task *model.NativeTask, provider Provider) (*model.NativeTask, error) {
	owner := task.DispatchOwner
	fields := log.Fields{"lane": "native", "task_id": task.ID, "group": task.GroupID, "model": task.Model, "scope": task.QueueScope}
	call := func(command balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
		command.Group = task.GroupID
		command.BillingOperationID = task.BillingOperationID
		return e.Wallet.Prepayment(ctx, command)
	}
	fail := func(code string) (*model.NativeTask, error) {
		if err := model.FailNativeDispatch(e.DB, task.ID, task.GroupID, task.TokenID, owner, code); err != nil {
			return e.storedQueued(task), err
		}
		log.WithFields(fields).WithField("error_code", code).Warn("native queued task failed before it was sent; refunding its hold")
		wakeDispatcher()
		failed := e.storedQueued(task)
		return failed, e.SyncBilling(ctx, failed)
	}
	requeue := func(backoff time.Duration, reason string) (*model.NativeTask, error) {
		wake := time.Now().Add(e.queueTimeout())
		if task.EnqueuedAt != nil {
			wake = task.EnqueuedAt.Add(e.queueTimeout())
		}
		if err := model.ReturnNativeDispatch(e.DB, task.ID, task.GroupID, task.TokenID, owner, wake.Add(time.Second)); err != nil {
			return e.storedQueued(task), err
		}
		queue.backOff(task.GroupID, backoff)
		log.WithFields(fields).WithField("reason", reason).Info("native queued task back in the queue")
		return e.storedQueued(task), nil
	}
	route, err := dispatchRoute(task)
	if err != nil {
		return fail(model.ModelUnavailableWhileQueuedCode)
	}
	if provider == nil {
		resolver, ok := e.QueueProviders[route.Provider]
		if !ok || resolver.Resolve == nil {
			return fail(model.ModelUnavailableWhileQueuedCode)
		}
		provider, err = resolver.Resolve(ctx, task)
		if errors.Is(err, ErrChannelUnusable) {
			// Never rerouted: the quote froze this channel (spec §6.4).
			return fail(model.ModelUnavailableWhileQueuedCode)
		}
		if err != nil || provider == nil {
			return requeue(transientBackoff, "channel unreadable")
		}
	}
	contract, err := nativeresult.CompileTaskContract([]byte(task.FrozenContract))
	if err != nil {
		return fail(model.ModelUnavailableWhileQueuedCode)
	}
	walletCtx, cancelWallet := context.WithTimeout(balance.WithSubmitRetry(ctx), submitWalletBudget)
	defer cancelWallet()
	attemptID := task.BillingOperationID + ":1"
	receipt, err := e.Wallet.Prepayment(walletCtx, balance.PrepaymentCommand{Action: "begin_attempt", Group: task.GroupID,
		BillingOperationID: task.BillingOperationID, RouteID: route.RouteID, AttemptID: attemptID})
	var limit *balance.TaskLimitError
	switch {
	case errors.As(err, &limit) || errors.Is(err, balance.ErrPrepaymentTooManyActiveTasks):
		if limit != nil {
			queue.rememberLimit(task.GroupID, limit.ActiveLimit)
		}
		return requeue(activeLimitBackoff, "active task limit")
	case errors.Is(err, balance.ErrPrepaymentModelPaused):
		return requeue(pausedModelBackoff, "model paused")
	case errors.Is(err, balance.ErrPrepaymentInsufficientBalance):
		// The balance went below zero while the task waited (spec §4.7).
		return fail(model.InsufficientBalanceCode)
	case errors.Is(err, balance.ErrPrepaymentRejected):
		// The wallet refused the claim and claimed nothing. A refunded hold
		// (its 2-hour backstop) reads as a queue timeout (spec §6.5).
		if current, getErr := call(balance.PrepaymentCommand{Action: "get"}); getErr == nil && current.Status != "pending" {
			return fail(model.QueueTimeoutCode)
		}
		return fail(model.NotStartedCode)
	case err != nil:
		// Unknown outcome: the claim may still land. The task stays
		// dispatching; recovery fails and refunds it when the lease ends
		// (spec §6.3, §6.6). It is never put back in the queue.
		log.WithFields(fields).WithError(err).Warn("native queued task: wallet claim outcome unknown; left to the lease")
		return e.storedQueued(task), err
	case !receipt.Claimed:
		// A lost reply never authorizes another send (spec §4.2, §6.6).
		return fail(model.NotStartedCode)
	}
	if err = model.StartNativeDispatchSubmission(e.DB, task.ID, task.GroupID, task.TokenID, owner, time.Now()); err != nil {
		// The lease ran out or recovery ended the claim: never send. Recovery
		// refunds the claimed attempt (refundNotStarted).
		log.WithFields(fields).WithError(err).Warn("native queued task claimed in the wallet after its lease; not sent")
		return e.storedQueued(task), err
	}
	return e.submitClaimed(ctx, task, contract, provider, nil, call)
}

// RunDispatcher claims and sends queued tasks until ctx ends (spec §5.7): a
// round every second, or at once when a task is queued or leaves provider
// capacity. On shutdown it stops claiming and waits for the tasks it claimed.
func RunDispatcher(ctx context.Context, engine func() (*Engine, bool)) {
	var running sync.WaitGroup
	defer running.Wait()
	ticker := time.NewTicker(dispatchRoundInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-queue.wake:
		}
		if e, ok := engine(); ok {
			e.DispatchRound(ctx, &running)
		}
	}
}

// DispatchRound claims, in every scope with waiting tasks, as many tasks as
// it has room for and sends each in its own goroutine, detached from ctx with
// the submission timeout. running tracks them.
func (e *Engine) DispatchRound(ctx context.Context, running *sync.WaitGroup) {
	scopes, err := model.NativeQueueScopesWaiting(e.DB)
	if err != nil {
		log.WithError(err).Warn("native queue: waiting scopes unreadable")
		return
	}
	for _, scope := range scopes {
		for ctx.Err() == nil {
			task, noRoom, err := e.claim(scope, "")
			if err != nil {
				log.WithFields(log.Fields{"lane": "native", "scope": scope}).WithError(err).Warn("native queue claim failed")
				break
			}
			if task == nil {
				if noRoom {
					e.expedite(scope)
				}
				break
			}
			running.Add(1)
			go func(task *model.NativeTask) {
				defer running.Done()
				dispatchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), submitTimeout)
				defer cancel()
				_, _ = e.dispatchClaimed(dispatchCtx, task, nil)
			}(task)
		}
	}
}
