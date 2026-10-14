package nativetask

import (
	"context"
	"encoding/json"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
)

// Expired counts tasks this pass failed at model.AsyncGenerationDeadline and
// Abandoned reservations it failed as not_started; both are also counted as
// Advanced or Deferred.
type RecoveryReport struct{ Claimed, Advanced, Deferred, Expired, Abandoned int }

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

// reservationGrace is how long a reservation that holds money must stay quiet,
// on the gateway and in the wallet, before recovery fails it as not_started
// and refunds it (owner decision 2026-10-13: 5 minutes). It exceeds
// submitTimeout, which exceeds submitWalletBudget, so the submission that
// made the reservation is over; a later same-ID retry still racing it loses
// the compare-and-set and never reaches the provider.
const reservationGrace = 5 * time.Minute

// reservationRefundBudget bounds the wallet calls that refund an abandoned
// reservation, inside the 2-minute recovery lease.
const reservationRefundBudget = 60 * time.Second

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

// awaitingProvider is an accepted task with no result yet. Its created_at is
// the acceptance time.
func awaitingProvider(task *model.NativeTask) bool {
	return task.UpstreamID != "" && (task.Status == "queued" || task.Status == "running")
}

// unconfirmedSubmission is a submission the provider never confirmed: no
// upstream ID, and its outcome was unknown or the submitting process died.
// Its created_at is the time the submission started.
func unconfirmedSubmission(task *model.NativeTask) bool {
	return task.UpstreamID == "" && (task.Status == "submission_unknown" || task.Status == "submitting")
}

// underGenerationDeadline reports the states model.AsyncGenerationDeadline
// applies to: waiting for the provider, before or after it accepted.
func underGenerationDeadline(task *model.NativeTask) bool {
	return awaitingProvider(task) || unconfirmedSubmission(task)
}

// expireGeneration applies model.AsyncGenerationDeadline after this pass's
// poll, so a result the provider finished in time is still delivered; past the
// deadline it fires whatever the poll returned. The order is durable failure,
// refund, then one best-effort cancel: neither can undo the failure and the
// cancel never delays the refund. expired reports whether this call ended the
// task; err is a refund error, which the next pass retries through Poll and
// SyncBilling because the row stays failed and unsettled.
func (e *Engine) expireGeneration(ctx context.Context, task *model.NativeTask, now time.Time, resolve ResolvePoller) (bool, error) {
	if !underGenerationDeadline(task) || now.Before(task.CreatedAt.Add(model.AsyncGenerationDeadline)) {
		return false, nil
	}
	if unconfirmedSubmission(task) {
		return e.expireUnconfirmed(ctx, task, now)
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

// expireUnconfirmed ends a submission the provider never confirmed within
// model.AsyncGenerationDeadline (owner rule 2026-10-08): durable failure with
// generation_timeout, then a full refund in one wallet call (SyncBilling:
// settle failed/platform_failure, no execution_finished). There is no upstream
// ID, so nothing is polled or cancelled, and the task is never resubmitted.
// A refund error is retried by later passes: the row stays failed, unsettled.
func (e *Engine) expireUnconfirmed(ctx context.Context, task *model.NativeTask, now time.Time) (bool, error) {
	expired, err := model.ExpireUnconfirmedNativeSubmission(e.DB, task.ID, task.GroupID, task.TokenID, now)
	if err != nil || !expired {
		return false, err
	}
	log.WithFields(log.Fields{
		"lane": "native", "task_id": task.ID, "group": task.GroupID, "model": task.Model,
		"endpoint": task.Endpoint, "channel_id": task.ChannelID, "previous_status": task.Status,
	}).Warn("native task submission was not confirmed within the generation deadline; failed with generation_timeout")
	failed, err := model.GetNativeTask(e.DB, task.ID, task.GroupID, task.TokenID)
	if err == nil {
		err = e.SyncBilling(ctx, failed)
	}
	if err != nil {
		log.Warnf("native task %s unconfirmed submission timed out; refund deferred to recovery: %v", task.ID, err)
	}
	return true, err
}

// reservationQuietAt is when a reservation has been quiet for reservationGrace
// on both sides: since the gateway reserved it, since the wallet admitted its
// hold and, while its attempt is unknown, since the wallet recorded the claim.
// The wallet's times keep a same-ID retry that just admitted or claimed from
// being abandoned. ok is false when a wallet time it needs is unreadable.
func reservationQuietAt(task *model.NativeTask, hold balance.PrepaymentReceipt) (time.Time, bool) {
	last := task.CreatedAt
	admitted, ok := hold.AdmittedAt()
	if !ok {
		return time.Time{}, false
	}
	if admitted.After(last) {
		last = admitted
	}
	if hold.Submission() == "unknown" {
		claimed, ok := hold.UnknownAt()
		if !ok {
			return time.Time{}, false
		}
		if claimed.After(last) {
			last = claimed
		}
	}
	return last.Add(reservationGrace), true
}

// abandonReservation ends a reservation that holds money but was never sent
// to a provider (S1, owner decision 2026-10-13): the gateway calls a provider
// only after the reserved->submitting compare-and-set, so a reserved row is
// durable proof that nothing was submitted. It acts only on this pass's wallet
// receipt (task.BillingReceiptJSON, saved by Poll's get) when the hold is
// pending, its submission is unknown or not_submitted, and both sides have
// been quiet for reservationGrace at now. It first fails the row (reserved ->
// failed, not_started) with the same guard as the submit transition, so a
// Submit still in flight can no longer call the provider, and only then
// refunds it (refundNotStarted: release the attempt if listed, then settle
// failed/not_accepted, which never pauses the model). abandoned reports
// whether this call failed the row; err is a refund error, which later passes
// retry through SyncBilling because the row stays failed and unsettled.
func (e *Engine) abandonReservation(ctx context.Context, task *model.NativeTask, now time.Time) (bool, error) {
	var hold balance.PrepaymentReceipt
	if json.Unmarshal([]byte(task.BillingReceiptJSON), &hold) != nil || hold.Status != "pending" {
		return false, nil
	}
	fields := log.Fields{"lane": "native", "task_id": task.ID, "group": task.GroupID, "model": task.Model, "submission": hold.Submission()}
	switch hold.Submission() {
	case "unknown", "not_submitted":
	case "accepted":
		// The wallet saw a provider acceptance this gateway never recorded:
		// never refund it as not accepted.
		log.WithFields(fields).Error("reserved native task has an accepted wallet attempt; left for an operator")
		return false, nil
	default:
		return false, nil
	}
	quietAt, ok := reservationQuietAt(task, hold)
	if !ok {
		log.WithFields(fields).Warn("reserved native task kept: wallet hold times unreadable")
		return false, nil
	}
	if now.Before(quietAt) {
		return false, nil
	}
	abandoned, err := model.AbandonNativeReservation(e.DB, task.ID, task.GroupID, task.TokenID, now.Add(-reservationGrace))
	if err != nil || !abandoned {
		return false, err
	}
	log.WithFields(fields).Warn("native reservation was never submitted; failed with not_started, refunding its hold")
	refundCtx, cancel := context.WithTimeout(ctx, reservationRefundBudget)
	defer cancel()
	failed, err := model.GetNativeTask(e.DB, task.ID, task.GroupID, task.TokenID)
	if err == nil {
		err = e.SyncBilling(refundCtx, failed)
	}
	if err != nil {
		log.Warnf("native task %s not_started refund deferred to recovery: %v", task.ID, err)
	}
	return true, err
}

// reservationWake is when recovery should next look at a reservation that is
// not yet quiet: one second past reservationQuietAt, or past the gateway's
// grace when the wallet's times are unknown.
func reservationWake(task *model.NativeTask) time.Time {
	wake := task.CreatedAt.Add(reservationGrace)
	var hold balance.PrepaymentReceipt
	if json.Unmarshal([]byte(task.BillingReceiptJSON), &hold) == nil {
		if quietAt, ok := reservationQuietAt(task, hold); ok {
			wake = quietAt
		}
	}
	return wake.Add(time.Second)
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
		if !expired && workErr == nil && e.AbandonReservations && fresh.Status == "reserved" {
			abandoned, abandonErr := e.abandonReservation(ctx, fresh, current)
			if abandoned {
				report.Abandoned++
				fresh.Status = "failed"
			}
			workErr = abandonErr
		}
		if fresh.Status == "reserved" && balance.WalletConfirmedNotFound(workErr) && age > staleReservationAge {
			// Nothing is held or owed for it, and a retry creates a new
			// reservation. Only the application's own "Prepayment not found"
			// proves that: a bare 404 (a platform page during a deploy) could
			// delete a reservation whose hold the stop-loss must still refund.
			if released, err := model.ReleaseNativeReservation(e.DB, task.ID, task.GroupID, task.TokenID, current.Add(-staleReservationAge)); err == nil && released {
				report.Advanced++
				continue
			}
		}
		release := now.Add(time.Since(started))
		next := release.Add(recoveryDelay(age, workErr != nil))
		if !expired && underGenerationDeadline(fresh) {
			// Check once more right at the deadline instead of up to ~2 minutes
			// late. One second past it: leases are whole seconds, and a claim just
			// before the deadline would only poll and reschedule again.
			if deadline := fresh.CreatedAt.Add(model.AsyncGenerationDeadline).Add(time.Second); deadline.After(release) && deadline.Before(next) {
				next = deadline
			}
		}
		if e.AbandonReservations && fresh.Status == "reserved" {
			// Likewise, check a reservation again right when it becomes quiet.
			if wake := reservationWake(fresh); wake.After(release) && wake.Before(next) {
				next = wake
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
