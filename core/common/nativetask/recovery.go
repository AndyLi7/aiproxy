package nativetask

import (
	"context"
	"errors"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
)

// Expired counts tasks this pass failed at model.AsyncGenerationDeadline; they
// are also counted as Advanced or Deferred.
type RecoveryReport struct{ Claimed, Advanced, Deferred, Expired int }

// NativeTaskDeadline bounds how long a received result may stay undeliverable
// (archive or wallet failures) before it is failed and refunded. Waiting for
// the provider ends much sooner, at model.AsyncGenerationDeadline.
const NativeTaskDeadline = 6 * time.Hour

// providerCancelTimeout bounds the one best-effort cancel sent upstream after
// a generation timeout.
const providerCancelTimeout = 10 * time.Second

// staleReservationAge is how long a reservation the wallet has no record of is
// kept before recovery deletes it. Admission takes seconds; this only removes
// reservations whose admission was refused or never reached the wallet.
const staleReservationAge = time.Hour

// recoveryDelay backs off with task age so a stuck task stops being re-claimed
// every minute: about an eighth of its age, between 2s (1m after an error)
// and 30 minutes. Customer reads never poll, so young tasks are polled often.
func recoveryDelay(age time.Duration, failed bool) time.Duration {
	delay := 2 * time.Second
	if failed {
		delay = time.Minute
	}
	if scaled := age / 8; scaled > delay {
		delay = scaled
	}
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	return delay
}

// awaitingProvider is an accepted task with no result yet, the only state the
// generation deadline applies to. Its created_at is the acceptance time.
// Unaccepted tasks (no upstream ID) keep the wallet's submission timeout.
func awaitingProvider(task *model.NativeTask) bool {
	return task.UpstreamID != "" && (task.Status == "queued" || task.Status == "running")
}

// expireGeneration applies model.AsyncGenerationDeadline after this pass's
// poll, so a result the provider finished in time is still delivered; past the
// deadline it fires whatever the poll returned. The order is durable failure,
// refund, then one best-effort cancel: neither can undo the failure and the
// cancel never delays the refund. expired reports whether this call ended the
// task; err is a refund error, which the next pass retries through Poll and
// SyncBilling because the row stays failed and unsettled.
func (e *Engine) expireGeneration(ctx context.Context, task *model.NativeTask, now time.Time, resolve ResolvePoller) (bool, error) {
	if !awaitingProvider(task) || now.Before(task.CreatedAt.Add(model.AsyncGenerationDeadline)) {
		return false, nil
	}
	expired, err := model.ExpireNativeGeneration(e.DB, task.ID, task.GroupID, task.TokenID, now)
	if err != nil || !expired {
		return false, err
	}
	failed, err := model.GetNativeTask(e.DB, task.ID, task.GroupID, task.TokenID)
	if err == nil {
		err = e.SyncBilling(ctx, failed)
	}
	if err != nil {
		log.Warnf("native task %s generation timed out; refund deferred to recovery: %v", task.ID, err)
	}
	// Only the call that ended the task cancels, so a cancel is sent once.
	cancelUpstream(ctx, task, resolve)
	return true, err
}

// cancelUpstream asks the provider once to stop an expired request. It only
// uses the task's own bound credential (ResolvePoller refuses a rotated or
// retargeted channel), never retries, and only logs the outcome.
func cancelUpstream(ctx context.Context, task *model.NativeTask, resolve ResolvePoller) {
	if resolve == nil {
		log.Warnf("native task %s generation timed out; provider cancel unavailable", task.ID)
		return
	}
	// Detached so a worker shutting down still sends the single cancel.
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), providerCancelTimeout)
	defer cancel()
	provider, err := resolve(cancelCtx, task)
	canceller, ok := provider.(Canceller)
	if err != nil || provider == nil || !ok {
		log.Warnf("native task %s generation timed out; provider cancel unavailable", task.ID)
		return
	}
	outcome, err := canceller.CancelNative(cancelCtx, task.Endpoint, task.UpstreamID)
	if err != nil {
		log.Warnf("native task %s generation timed out; provider cancel failed: %v", task.ID, err)
		return
	}
	log.Infof("native task %s generation timed out; provider cancel %s", task.ID, outcome)
}

// RecoverOnce is safe to invoke from multiple workers: durable leases bound
// concurrent polling. It never calls Submit or retries an uncertain paid call.
func (e *Engine) RecoverOnce(ctx context.Context, owner string, now time.Time, resolve ResolvePoller, archive ArchiveFunc) (RecoveryReport, error) {
	report := RecoveryReport{}
	if e == nil || e.DB == nil || e.Wallet == nil {
		return report, ErrUnavailable
	}
	started := time.Now()
	for iteration := 0; iteration < 20; iteration++ {
		current := now.Add(time.Since(started))
		tasks, err := model.ClaimNativeRecovery(e.DB, owner, current, 1)
		if err != nil {
			return report, err
		}
		if len(tasks) == 0 {
			break
		}
		task := tasks[0]
		report.Claimed++
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		workCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		_, workErr := e.Poll(workCtx, task.ID, task.GroupID, task.TokenID, resolve, archive)
		cancel()
		// Decide on the row as this poll left it, never on the claimed snapshot:
		// the poll may have saved a result, which must not be expired.
		fresh, readErr := model.GetNativeTask(e.DB, task.ID, task.GroupID, task.TokenID)
		if readErr != nil {
			fresh = &task
			if workErr == nil {
				workErr = readErr
			}
		}
		current = now.Add(time.Since(started))
		age := current.Sub(fresh.CreatedAt)
		expired, expireErr := e.expireGeneration(ctx, fresh, current, resolve)
		switch {
		case expired:
			report.Expired++
			workErr = expireErr
		case expireErr != nil:
			workErr = expireErr
		case workErr != nil && age > NativeTaskDeadline && fresh.Status == "result_received":
			// Backstop: a received result still undeliverable after the long
			// deadline is ended and refunded instead of retried forever.
			if model.FailAcceptedNativeTask(e.DB, task.ID, task.GroupID, task.TokenID, "upstream_result_rejected") == nil {
				if failed, err := model.GetNativeTask(e.DB, task.ID, task.GroupID, task.TokenID); err == nil {
					workErr = e.SyncBilling(ctx, failed)
				}
			}
		}
		if fresh.Status == "reserved" && errors.Is(workErr, balance.ErrPrepaymentNotFound) && age > staleReservationAge {
			// Nothing is held or owed for it, and a retry creates a new reservation.
			if released, err := model.ReleaseNativeReservation(e.DB, task.ID, task.GroupID, task.TokenID, current.Add(-staleReservationAge)); err == nil && released {
				report.Advanced++
				continue
			}
		}
		release := now.Add(time.Since(started))
		next := release.Add(recoveryDelay(age, workErr != nil))
		if !expired && awaitingProvider(fresh) {
			// Poll once more right at the deadline instead of up to ~2 minutes
			// late. One second past it: leases are whole seconds, and a claim just
			// before the deadline would only poll and reschedule again.
			if deadline := fresh.CreatedAt.Add(model.AsyncGenerationDeadline).Add(time.Second); deadline.After(release) && deadline.Before(next) {
				next = deadline
			}
		}
		if workErr != nil {
			report.Deferred++
		} else {
			report.Advanced++
		}
		if err = model.ReleaseNativeRecovery(e.DB, task.ID, owner, next); err != nil {
			return report, err
		}
	}
	return report, nil
}
