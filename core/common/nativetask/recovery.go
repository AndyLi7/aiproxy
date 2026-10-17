package nativetask

import (
	"context"
	"encoding/json"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// Expired counts tasks this pass failed at model.AsyncGenerationDeadline and
// Abandoned reservations it failed as not_started; both are also counted as
// Advanced or Deferred.
type nativeRecoveryOwnerKey struct{}

// Carries the exact claim, so a reused worker name cannot borrow a newer lease.
type nativeRecoveryExecutionUntilKey struct{}

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
	expired, err := model.ExpireNativeGeneration(e.DB.WithContext(ctx), task.ID, task.GroupID, task.TokenID, now)
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
	expired, err := model.ExpireUnconfirmedNativeSubmission(e.DB.WithContext(ctx), task.ID, task.GroupID, task.TokenID, now)
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
	abandoned, err := model.AbandonNativeReservationGeneration(e.DB, task, now.Add(-reservationGrace), quietAt)
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

// recoverQueued handles a waiting or dispatching task, the only work recovery
// does on them (spec §6.10): a waiting task past its queue timeout is failed
// (queue_timeout) and refunded; a dispatch claim past its lease is failed
// (not_started) and refunded, never put back in the queue (§6.6). Before
// that, nothing is done and the wallet is not called. Each move is a
// compare-and-set that a concurrent claim, cancel or holder wins or loses.
func (e *Engine) recoverQueued(ctx context.Context, task *model.NativeTask, now time.Time) error {
	fields := log.Fields{"lane": "native", "task_id": task.ID, "group": task.GroupID, "model": task.Model, "scope": task.QueueScope}
	var ended bool
	var err error
	switch task.Status {
	case model.NativeStatusWaiting:
		if now.Before(queuedSince(task).Add(e.queueTimeout())) {
			return nil
		}
		ended, err = model.ExpireNativeQueueWait(e.DB, task.ID, task.GroupID, task.TokenID, now.Add(-e.queueTimeout()))
		if ended {
			log.WithFields(fields).Warn("native task waited past the queue timeout; failed with queue_timeout, refunding its hold")
		}
	case model.NativeStatusDispatching:
		if task.DispatchUntil != nil && now.Before(*task.DispatchUntil) {
			return nil
		}
		ended, err = model.ExpireNativeDispatch(e.DB, task.ID, task.GroupID, task.TokenID, task.DispatchOwner, now)
		if ended {
			log.WithFields(fields).Warn("native dispatch claim lease ran out before the task was sent; failed with not_started, refunding its hold")
		}
	}
	if err != nil || !ended {
		return err
	}
	wakeDispatcher()
	refundCtx, cancel := context.WithTimeout(ctx, reservationRefundBudget)
	defer cancel()
	failed, err := model.GetNativeTask(e.DB, task.ID, task.GroupID, task.TokenID)
	if err == nil {
		err = e.SyncBilling(refundCtx, failed)
	}
	if err != nil {
		log.WithFields(fields).WithError(err).Warn("native queued task refund deferred to recovery")
	}
	return err
}

// queuedSince is when a task entered the queue.
func queuedSince(task *model.NativeTask) time.Time {
	if task.EnqueuedAt != nil {
		return *task.EnqueuedAt
	}
	return task.CreatedAt
}

// enqueueRecovered completes the queueing of a reserved task whose wallet hold
// is waiting: its process stopped between the admission and the enqueue
// (spec §6.7). The task keeps its admission time as its place. It waits
// reservedEnqueueGrace after the admission, so a live request enqueues (and
// fast-paths) its own task. The stop-loss never touches such a hold.
func (e *Engine) enqueueRecovered(task *model.NativeTask, now time.Time) (bool, time.Time) {
	var hold balance.PrepaymentReceipt
	if json.Unmarshal([]byte(task.BillingReceiptJSON), &hold) != nil || hold.Status != "pending" || hold.Submission() != model.NativeStatusWaiting {
		return false, time.Time{}
	}
	admitted, ok := hold.AdmittedAt()
	if !ok {
		admitted = task.CreatedAt
	}
	if due := admitted.Add(reservedEnqueueGrace); now.Before(due) {
		return false, due
	}
	scope := task.QueueScope
	if scope == "" {
		route, err := dispatchRoute(task)
		if err != nil {
			return false, time.Time{}
		}
		scope = QueueScope(route.Provider)
	}
	wake := admitted.Add(e.queueTimeout()).Add(time.Second)
	if err := model.WithNativeReservationGeneration(e.DB, task, func(tx *gorm.DB) error {
		return model.EnqueueNativeTask(tx, task.ID, task.GroupID, task.TokenID, scope, admitted, wake)
	}); err != nil {
		return false, time.Time{}
	}
	log.WithFields(log.Fields{"lane": "native", "task_id": task.ID, "group": task.GroupID, "model": task.Model, "scope": scope}).
		Warn("reserved native task with a waiting hold enqueued by recovery")
	wakeDispatcher()
	return true, time.Time{}
}

// inFlight reports a status that occupies provider capacity.
func inFlight(status string) bool {
	for _, candidate := range model.NativeInFlightStatuses {
		if status == candidate {
			return true
		}
	}
	return false
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
		claimOwner := task.RecoveryOwner
		recoveryCtx := context.WithValue(context.WithValue(ctx, nativeRecoveryOwnerKey{}, claimOwner), nativeRecoveryExecutionUntilKey{}, task.RecoveryUntil)
		report.Claimed++
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		var handoff *model.NativeRecoveryHandoffHandle
		if task.Status != "reserved" {
			handoff, err = model.PrepareNativeRecoveryHandoff(recoveryCtx, e.DB, &task)
			if err != nil {
				return report, err
			}
			if handoff != nil {
				accepted, acceptErr := handoff.Accept(recoveryCtx)
				if acceptErr != nil {
					return report, acceptErr
				}
				if !accepted {
					return report, model.ErrNativeTaskConflict
				}
			}
		}
		// A normal function return observes all defers owned by this body.
		// Panic/Goexit never reaches release/exit; no recovery fabricates success.
		outcome := e.recoverClaim(recoveryCtx, task, now, started, current, resolve, archive, &report)
		if outcome.removed {
			continue
		}
		if err = e.releaseRecoveryHandoff(task.ID, task.GroupID, task.TokenID, claimOwner, outcome.fresh, now.Add(time.Since(started)), outcome.age, outcome.expired, outcome.failed, outcome.enqueueAt, handoff); err != nil {
			return report, err
		}

	}
	return report, nil
}

type recoveryClaimOutcome struct {
	fresh                    *model.NativeTask
	age                      time.Duration
	expired, failed, removed bool
	enqueueAt                time.Time
}

// recoverClaim owns one claim's existing business work and cleanup. Its caller
// alone records normal completion; it adds no goroutine or timeout window.
func (e *Engine) recoverClaim(recoveryCtx context.Context, task model.NativeTask, now, started, current time.Time, resolve ResolvePoller, archive ArchiveFunc, report *RecoveryReport) recoveryClaimOutcome {
	claimOwner := task.RecoveryOwner
	recoveryCtx = model.WithNativeRecoveryOwner(recoveryCtx, claimOwner)
	var releaseEvidence model.NativeReservationRelease
	var releaseEvidenceErr error
	if task.Status == "reserved" {
		releaseEvidence, releaseEvidenceErr = model.CaptureNativeReservationRelease(e.DB, &task, claimOwner)
	}
	var workErr error
	queued := task.Status == model.NativeStatusWaiting || task.Status == model.NativeStatusDispatching
	if queued {
		workErr = e.recoverQueued(recoveryCtx, &task, current)
	} else {
		func() {
			workCtx, cancel := context.WithTimeout(recoveryCtx, 90*time.Second)
			defer cancel()
			workCtx = context.WithValue(workCtx, deliveryLeaseKey{}, deliveryLease{owner: claimOwner, parent: recoveryCtx})
			_, workErr = e.Poll(workCtx, task.ID, task.GroupID, task.TokenID, resolve, archive)
		}()
	}
	// Lost owners cannot run timeout or billing cleanup against replacement work.
	if err := model.CheckNativeRecovery(e.DB.WithContext(recoveryCtx), task.ID); err != nil || recoveryCtx.Err() != nil {
		report.Deferred++
		return recoveryClaimOutcome{fresh: &task, failed: true}
	}
	// Decide on the row as this poll left it, never on the claimed snapshot:
	// the poll may have saved a result, which must not be expired.
	fresh, readErr := model.GetNativeTask(e.DB, task.ID, task.GroupID, task.TokenID)
	if readErr != nil {
		fresh = &task
		if workErr == nil {
			workErr = readErr
		}
	}
	if fresh.CreatedAt.Equal(task.CreatedAt) {
		fresh.AdmissionGeneration = releaseEvidence.GenerationID
		fresh.AdmissionRecoveryOwner = claimOwner
	}
	current = now.Add(time.Since(started))
	age := current.Sub(fresh.CreatedAt)
	expired, expireErr := e.expireGeneration(recoveryCtx, fresh, current, resolve)
	switch {
	case expired:
		report.Expired++
		workErr = expireErr
	case expireErr != nil:
		workErr = expireErr
	case workErr != nil && age > NativeTaskDeadline && fresh.Status == "result_received":
		// Backstop: a received result still undeliverable after the long
		// deadline is ended and refunded instead of retried forever.
		if model.FailAcceptedNativeTask(e.DB.WithContext(recoveryCtx), task.ID, task.GroupID, task.TokenID, "upstream_result_rejected") == nil {
			if failed, err := model.GetNativeTask(e.DB, task.ID, task.GroupID, task.TokenID); err == nil {
				workErr = e.SyncBilling(recoveryCtx, failed)
			}
		}
	}
	var enqueueAt time.Time
	if !expired && workErr == nil && fresh.Status == "reserved" {
		// A reserved task whose hold is queued is enqueued, never abandoned.
		var enqueued bool
		if enqueued, enqueueAt = e.enqueueRecovered(fresh, current); enqueued {
			fresh.Status = model.NativeStatusWaiting
		}
	}
	if !expired && workErr == nil && e.AbandonReservations && fresh.Status == "reserved" && enqueueAt.IsZero() {
		abandoned, abandonErr := e.abandonReservation(recoveryCtx, fresh, current)
		if abandoned {
			report.Abandoned++
			fresh.Status = "failed"
		}
		workErr = abandonErr
	}
	if releaseEvidenceErr == nil && fresh.Status == "reserved" && balance.WalletConfirmedNotFound(workErr) && age > staleReservationAge {
		// Nothing is held or owed for it, and a retry creates a new
		// reservation. Only the application's own "Prepayment not found"
		// proves that: a bare 404 (a platform page during a deploy) could
		// delete a reservation whose hold the stop-loss must still refund.
		if released, err := model.ReleaseNativeReservation(e.DB, task.ID, task.GroupID, task.TokenID, current.Add(-staleReservationAge), releaseEvidence); err == nil && released {
			report.Advanced++
			return recoveryClaimOutcome{removed: true}
		}
	}
	if inFlight(task.Status) && !inFlight(fresh.Status) {
		// A place in the provider's capacity was freed.
		wakeDispatcher()
	}
	if workErr != nil {
		report.Deferred++
	} else {
		report.Advanced++
	}
	return recoveryClaimOutcome{fresh: fresh, age: age, expired: expired, failed: workErr != nil, enqueueAt: enqueueAt}
}

// releaseRecovery releases recovery's lease with the next time to look at the
// task, computed from its status (spec §6.10). The release only applies while
// the status is the one the time was computed from: a queue move meanwhile
// (enqueue, claim, return) is re-read and the time recomputed, so a queue
// timeout or a lease is never overwritten. owner is the original unique claim
// token; rereading fresh may change scheduling, never this ownership evidence.
func (e *Engine) releaseRecovery(id, group string, token int, owner string, fresh *model.NativeTask, release time.Time, age time.Duration, expired, failed bool, enqueueAt time.Time) error {
	return e.releaseRecoveryHandoff(id, group, token, owner, fresh, release, age, expired, failed, enqueueAt, nil)
}

func (e *Engine) releaseRecoveryHandoff(id, group string, token int, owner string, fresh *model.NativeTask, release time.Time, age time.Duration, expired, failed bool, enqueueAt time.Time, handoff *model.NativeRecoveryHandoffHandle) error {
	releaseAt := func(status string, next time.Time) (bool, error) {
		if handoff == nil {
			if status == "" {
				err := model.ReleaseNativeRecovery(e.DB, id, owner, next)
				return err == nil, err
			}
			return model.ReleaseNativeRecoveryAt(e.DB, id, owner, status, next)
		}
		// This only bounds evidence persistence after owned work has returned; it
		// neither renews the claim nor authorizes business work after cancellation.
		exitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return handoff.ReleaseAndRecordExit(exitCtx, fresh, next, failed)
	}

	for attempt := 0; ; attempt++ {
		next := e.recoveryNext(fresh, release, age, expired, failed, enqueueAt)
		if attempt == 2 && handoff == nil {
			_, err := releaseAt("", next)
			return err
		}
		released, err := releaseAt(fresh.Status, next)
		if err != nil || released {
			return err
		}
		if attempt == 2 {
			return model.ErrNativeTaskConflict
		}
		reread, err := model.GetNativeTask(e.DB, id, group, token)
		if err != nil {
			if handoff != nil {
				return err
			}
			_, err := releaseAt("", next)
			return err
		}
		fresh = reread
	}
}

// recoveryNext is when recovery should next look at a task.
func (e *Engine) recoveryNext(fresh *model.NativeTask, release time.Time, age time.Duration, expired, failed bool, enqueueAt time.Time) time.Time {
	soonest := func(at time.Time) time.Time {
		if !at.After(release) {
			// Due already: look again shortly, later after an error.
			return release.Add(recoveryDelay(0, failed))
		}
		return at
	}
	switch fresh.Status {
	case model.NativeStatusWaiting:
		// Only its queue timeout concerns recovery.
		return soonest(queuedSince(fresh).Add(e.queueTimeout()).Add(time.Second))
	case model.NativeStatusDispatching:
		if fresh.DispatchUntil == nil {
			return release.Add(recoveryDelay(0, failed))
		}
		return soonest(fresh.DispatchUntil.Add(time.Second))
	}
	next := release.Add(recoveryDelay(age, failed))
	if !expired && underGenerationDeadline(fresh) {
		// Check once more right at the deadline instead of up to ~2 minutes
		// late. One second past it: leases are whole seconds, and a claim just
		// before the deadline would only poll and reschedule again.
		if deadline := fresh.CreatedAt.Add(model.AsyncGenerationDeadline).Add(time.Second); deadline.After(release) && deadline.Before(next) {
			next = deadline
		}
	}
	if fresh.Status == "reserved" {
		if !enqueueAt.IsZero() && enqueueAt.After(release) && enqueueAt.Before(next) {
			next = enqueueAt
		}
		if e.AbandonReservations {
			// Likewise, check a reservation again right when it becomes quiet.
			if wake := reservationWake(fresh); wake.After(release) && wake.Before(next) {
				next = wake
			}
		}
	}
	if ceiling := release.Add(queuedPollCeiling); next.After(ceiling) && inFlight(fresh.Status) && e.scopeHasWaiting(fresh.QueueScope) {
		// Tasks wait in this scope: notice finished work within seconds so
		// its place is freed (spec §5.8). Only work in flight: a finished
		// task waiting for its bill holds no place, and polling it sends
		// serialized wallet writes (execution_finished, delivered).
		next = ceiling
	}
	return next
}
