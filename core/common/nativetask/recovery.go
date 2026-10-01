package nativetask

import (
	"context"
	"github.com/labring/aiproxy/core/model"
	"time"
)

type RecoveryReport struct{ Claimed, Advanced, Deferred int }

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
		delay := 15 * time.Second
		if workErr != nil {
			delay = time.Minute
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
