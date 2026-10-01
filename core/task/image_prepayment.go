package task

import (
	"context"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/imageprepayment"
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
	"time"
)

// Covers failed and unknown submissions too; these have no active usage lease.
func ImagePrepaymentRecoveryTask(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			recoverImagePrepayments(ctx)
		}
	}
}
func recoverImagePrepayments(ctx context.Context) {
	client, ok := balance.Default.(*balance.ExternalHTTP)
	if !ok || model.LogDB == nil || !model.LogDB.Migrator().HasColumn(&model.ImageTask{}, "billing_next_check_at") {
		return
	}
	var tasks []model.ImageTask
	now := time.Now().UTC()
	if err := model.LogDB.Where("prepayment_quote_json <> '' AND billing_settled = ? AND (billing_next_check_at IS NULL OR billing_next_check_at <= ?)", false, now).
		Order("billing_next_check_at ASC").Limit(10).Find(&tasks).Error; err != nil {
		log.Warn("image prepayment recovery scan unavailable")
		return
	}
	for i := range tasks {
		task := &tasks[i]
		// Claim a short lease using the same predicate. All remote effects are also
		// idempotent, so expiry/crashes cannot duplicate a debit or refund.
		won := model.LogDB.Model(&model.ImageTask{}).Where("id = ? AND (billing_next_check_at IS NULL OR billing_next_check_at <= ?)", task.ID, now).
			Update("billing_next_check_at", now.Add(2*time.Minute))
		if won.Error != nil || won.RowsAffected != 1 {
			continue
		}
		if _, err := imageprepayment.Sync(ctx, task); err != nil {
			log.WithField("task_id", task.ID).Warn("image prepayment state synchronization unavailable")
		}
	}
	// The app continues to collect late bills even after customer settlement.
	if err := client.RecoverPrepayments(ctx); err != nil {
		log.Warn("upstream billing recovery unavailable")
	}
}
