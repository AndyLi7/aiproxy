package model

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

// ErrImagePollClaimLost rejects late work, including unchanged but expired tokens.
var ErrImagePollClaimLost = errors.New("image poll claim lost")

// withImagePollClaim serializes task -> usage -> receipts and captures the current
// valid deadline. Same-token renewal is allowed; the caller's NextPollAt is a
// scheduling field, never authority. No network action is executed under these locks.
func withImagePollClaim(ctx context.Context, info *AsyncUsageInfo, observed *ImageTask, write func(*gorm.DB, *ImageTask, *AsyncUsageInfo) error) error {
	if info == nil || observed == nil || info.ID == 0 || info.ProcessingToken == "" || info.ImageTaskID != observed.ID || LogDB == nil {
		return ErrImagePollClaimLost
	}
	clock, err := asyncUsageLeaseClockSQL(LogDB)
	if err != nil {
		return err
	}
	return LogDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var task ImageTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", observed.ID).First(&task).Error; err != nil {
			return err
		}
		if imageRecoveryCoreHash(&task) != imageRecoveryCoreHash(observed) || task.ValidationContract != observed.ValidationContract || task.UpstreamID != observed.UpstreamID || task.UpstreamModel != observed.UpstreamModel || task.KeyFingerprint != observed.KeyFingerprint || task.ChannelType != observed.ChannelType || task.ArchiveRequired != observed.ArchiveRequired {
			return ErrImagePollClaimLost
		}
		var current AsyncUsageInfo
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND image_task_id=? AND group_id=? AND token_id=? AND status=? AND processing_token=?", info.ID, task.ID, task.GroupID, task.TokenID, AsyncUsageStatusPending, info.ProcessingToken).First(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrImagePollClaimLost
		}
		if err != nil {
			return err
		}
		if (task.UsageID != 0 && task.UsageID != current.ID) || current.LogID != info.LogID || current.RequestID != info.RequestID || current.ChannelID != info.ChannelID || current.Mode != info.Mode || current.BillingOperationID != info.BillingOperationID {
			return ErrImagePollClaimLost
		}
		var now int64
		if err := tx.Raw("SELECT " + clock).Scan(&now).Error; err != nil {
			return err
		}
		if now <= 0 || current.NextPollAt.IsZero() || current.NextPollAt.UnixMilli() <= now {
			return ErrImagePollClaimLost
		}
		deadline := current.NextPollAt.UnixMilli()
		if err := write(tx, &task, &current); err != nil {
			return err
		}
		status, token := current.Status, current.ProcessingToken
		var count int64
		if err := tx.Model(&AsyncUsageInfo{}).Where("id=? AND status=? AND processing_token=?", current.ID, status, token).Where("? > "+clock, deadline).Count(&count).Error; err != nil {
			return err
		}
		if count != 1 {
			return ErrImagePollClaimLost
		}
		return nil
	}, claimTxOptions(LogDB)...)
}

func ValidateClaimedImageTask(ctx context.Context, info *AsyncUsageInfo, task *ImageTask) error {
	return withImagePollClaim(ctx, info, task, func(_ *gorm.DB, current *ImageTask, _ *AsyncUsageInfo) error {
		if current.Status != task.Status {
			return ErrImagePollClaimLost
		}
		return nil
	})
}

// A winning terminal transaction releases only this poll claim. The caller may
// then use the existing terminal refund mechanism; that later wallet work is NOT
// fenced by this transaction and does not prove executor exit or use closure.
func SetClaimedImageTaskResult(ctx context.Context, info *AsyncUsageInfo, observed *ImageTask, status string, data []ImageOutput, taskError *ImageTaskError, metadata ...ImageResultMetadata) error {
	return withImagePollClaim(ctx, info, observed, func(tx *gorm.DB, task *ImageTask, current *AsyncUsageInfo) error {
		// A durable completed result still wins over a late failure from this
		// valid owner. Do not fail its outbox; the caller settles the saved result.
		if status == "failed" && task.Status == "completed" {
			return nil
		}
		if task.Status == "failed" {
			return failImagePollReceipt(tx, task, current, task.Error, "image generation failed")
		}
		if err := writeImageTaskResult(tx, task.ID, status, data, taskError, false, metadata...); err != nil {
			return err
		}
		if status != "failed" {
			return nil
		}
		return failImagePollReceipt(tx, task, current, taskError, "image generation failed")
	})
}

// FailClaimedImageTaskUsage rejects an invalid stored result without rewriting
// its durable task result, preserving the existing accounting-only failure rule.
func FailClaimedImageTaskUsage(ctx context.Context, info *AsyncUsageInfo, task *ImageTask, message string) error {
	return withImagePollClaim(ctx, info, task, func(tx *gorm.DB, current *ImageTask, usage *AsyncUsageInfo) error {
		if current.Status != task.Status {
			return ErrImagePollClaimLost
		}
		return failImagePollReceipt(tx, current, usage, nil, message)
	})
}
func failImagePollReceipt(tx *gorm.DB, task *ImageTask, current *AsyncUsageInfo, taskError *ImageTaskError, message string) error {
	at := time.Now().UTC()
	result := tx.Model(&AsyncUsageInfo{}).Where("id=? AND status=? AND processing_token=?", current.ID, AsyncUsageStatusPending, current.ProcessingToken).Updates(map[string]any{"status": AsyncUsageStatusFailed, "processing_token": "", "error": message, "updated_at": at})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrImagePollClaimLost
	}
	current.Status, current.ProcessingToken = AsyncUsageStatusFailed, ""
	// Keep the existing missing-log grace, but never overwrite an unrelated or
	// completed receipt. A missing legacy log is not evidence of complete delivery.
	if current.LogID <= 0 {
		return nil
	}
	var receipt Log
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", current.LogID).First(&receipt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if receipt.GroupID != current.GroupID || receipt.TokenID != current.TokenID || string(receipt.RequestID) != current.RequestID || string(receipt.BillingOperationID) != task.BillingOperationID || (current.BillingOperationID != "" && current.BillingOperationID != task.BillingOperationID) || (current.Mode != 0 && receipt.Mode != current.Mode) || receipt.AsyncUsageStatus != AsyncUsageStatusPending {
		return ErrImagePollClaimLost
	}
	changes := map[string]any{"async_usage_status": AsyncUsageStatusFailed, "code": 502, "safe_error": "Image generation failed", "content": message}
	if taskError != nil && PublicTaskErrorMessage(taskError.Code) != "" {
		changes["error_code"] = taskError.Code
	}
	result = tx.Model(&Log{}).Where("id=?", receipt.ID).Updates(changes)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrImagePollClaimLost
	}
	return nil
}

func SaveClaimedArchivedImage(ctx context.Context, info *AsyncUsageInfo, task *ImageTask, index int, output ImageOutput) error {
	if task == nil {
		return ErrImagePollClaimLost
	}
	copyTask := *task
	copyTask.Data = CloneImageOutputs(task.Data)
	err := withImagePollClaim(ctx, info, task, func(tx *gorm.DB, _ *ImageTask, _ *AsyncUsageInfo) error {
		return saveArchivedImage(tx, &copyTask, index, output)
	})
	if err == nil {
		task.Data = copyTask.Data
	}
	return err
}
func CompleteClaimedImageArchive(ctx context.Context, info *AsyncUsageInfo, task *ImageTask) error {
	return withImagePollClaim(ctx, info, task, func(tx *gorm.DB, _ *ImageTask, _ *AsyncUsageInfo) error { return completeImageArchive(tx, task) })
}
