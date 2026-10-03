package nativetask

import (
	"context"
	"github.com/labring/aiproxy/core/model"
	"time"
)

type RecoveryReport struct{ Claimed, Advanced, Deferred int }

// NativeTaskDeadline bounds how long an accepted task may stay undeliverable
// before it is failed and refunded.
const NativeTaskDeadline = 6 * time.Hour

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
		age := current.Sub(task.CreatedAt)
		if workErr != nil && age > NativeTaskDeadline && (task.Status == "queued" || task.Status == "running" || task.Status == "result_received") {
			// Still undeliverable after the deadline: end it and refund instead of
			// retrying forever. Unknown submissions settle through the wallet timeout.
			code := "upstream_task_failed"
			if task.Status == "result_received" {
				code = "upstream_result_rejected"
			}
			if model.FailAcceptedNativeTask(e.DB, task.ID, task.GroupID, task.TokenID, code) == nil {
				if failed, err := model.GetNativeTask(e.DB, task.ID, task.GroupID, task.TokenID); err == nil {
					workErr = e.SyncBilling(ctx, failed)
				}
			}
		}
		delay := recoveryDelay(age, workErr != nil)
		if workErr != nil {
			report.Deferred++
		} else {
			report.Advanced++
		}
		if err = model.ReleaseNativeRecovery(e.DB, task.ID, owner, now.Add(time.Since(started)).Add(delay)); err != nil {
			return report, err
		}
	}
	return report, nil
}
