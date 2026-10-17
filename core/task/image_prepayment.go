package task

import (
	"context"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/imageprepayment"
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// Covers failed and unknown submissions too; these have no active usage lease.
func ImagePrepaymentRecoveryTask(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	runRecoveryTicks(ctx, ticker.C, func(ctx context.Context) {
		expireUnconfirmedImageSubmissions(ctx, time.Now())
		if ctx.Err() == nil {
			recoverImagePrepayments(ctx)
		}
	})
}
func recoverImagePrepayments(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	_, ok := balance.Default.(*balance.ExternalHTTP)
	if !ok || model.LogDB == nil || !model.LogDB.Migrator().HasColumn(&model.ImageTask{}, "billing_next_check_at") {
		return
	}
	var tasks []model.ImageTask
	now := time.Now().UTC()
	if err := dueImagePrepayments(model.LogDB.WithContext(ctx), now).Find(&tasks).Error; err != nil {
		log.Warn("image prepayment recovery scan unavailable")
		return
	}
	for i := range tasks {
		if ctx.Err() != nil {
			return
		}
		task := &tasks[i]
		won, err := model.ClaimImagePrepaymentRecovery(ctx, task, now)
		if err != nil || !won {
			continue
		}

		handoff, err := model.PrepareImageRecoveryHandoff(ctx, task)
		if err != nil {
			log.WithField("task_id", task.ID).Warn("image recovery handoff unavailable")
			continue
		}
		if handoff != nil {
			accepted, err := handoff.Accept(ctx)
			if err != nil || !accepted {
				continue
			}
		}
		_, syncErr := imageprepayment.Sync(ctx, task)
		if syncErr != nil {
			log.WithField("task_id", task.ID).Warn("image prepayment state synchronization unavailable")
		}
		// A normal Sync return observes its owned cleanup. Never use a defer
		// to release on panic/Goexit or convert unknown work into success.
		exitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var releaseErr error
		if handoff != nil {
			releaseErr = handoff.ReleaseAndRecordExit(exitCtx, syncErr != nil)
		} else {
			releaseErr = model.ReleaseImagePrepaymentRecovery(exitCtx, task)
		}
		cancel()
		if releaseErr != nil {
			log.WithField("task_id", task.ID).Warn("image prepayment claim release unavailable")
		}

	}
}

// imagePrepaymentBatch bounds one prepayment recovery pass.
const imagePrepaymentBatch = 10

// dueImagePrepayments selects the unsettled prepaid tasks due for a wallet
// sync, oldest first. A task never checked has no billing_next_check_at and
// sorts by its creation time: PostgreSQL sorts NULLs last in ascending order,
// so ordering by the check time alone would starve those tasks whenever a full
// batch of scheduled ones is due.
func dueImagePrepayments(db *gorm.DB, now time.Time) *gorm.DB {
	return db.Where("prepayment_quote_json <> '' AND billing_settled = ? AND (billing_next_check_at IS NULL OR billing_next_check_at <= ?)", false, now).
		Order("COALESCE(billing_next_check_at, created_at) ASC").Limit(imagePrepaymentBatch)
}

// upstreamBillingInterval paces the application's provider bill matching. fal
// posts a bill about 10–20s after a task finishes, so customers see the final
// charge within about half a minute; the application backs off older tasks.
const upstreamBillingInterval = 10 * time.Second

// UpstreamBillingRecoveryTask asks the application to match provider bills,
// including late bills that arrive after customer settlement.
func UpstreamBillingRecoveryTask(ctx context.Context) {
	ticker := time.NewTicker(upstreamBillingInterval)
	defer ticker.Stop()
	runUpstreamBillingRecovery(ctx, ticker.C)
}

func runUpstreamBillingRecovery(ctx context.Context, ticks <-chan time.Time) {
	runRecoveryTicks(ctx, ticks, func(ctx context.Context) {
		client, ok := balance.Default.(*balance.ExternalHTTP)
		if !ok {
			return
		}
		if err := client.RecoverPrepayments(ctx); err != nil {
			log.Warn("upstream billing recovery unavailable")
		}
	})
}
