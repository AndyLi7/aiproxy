package model

import (
	"context"
	"errors"
	"reflect"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// UsesImageAccountingClaim excludes the existing prepaid Sync path from this
// batch. Its terminal wallet receipt and owner are a separate protocol. Failure
// to find a bound task never grants a legacy fallback.
func UsesImageAccountingClaim(info *AsyncUsageInfo) bool {
	if info == nil {
		return false
	}
	if info.MeasuredImage {
		return true
	}
	if info.ImageTaskID == "" {
		return false
	}
	var task ImageTask
	var stored AsyncUsageInfo
	if LogDB == nil || LogDB.Where("id=?", info.ID).First(&stored).Error != nil || stored.MeasuredImage || stored.ImageTaskID != info.ImageTaskID || stored.GroupID != info.GroupID || stored.TokenID != info.TokenID || stored.BillingOperationID != info.BillingOperationID {
		return true
	}
	// A prepaid quote cannot exempt an unbound or different billing operation.
	if LogDB != nil && LogDB.Where("id=? AND group_id=? AND token_id=?", info.ImageTaskID, info.GroupID, info.TokenID).First(&task).Error == nil && task.PrepaymentQuoteJSON != "" && task.BillingOperationID != "" && task.BillingOperationID == stored.BillingOperationID && (task.UsageID == 0 || task.UsageID == info.ID) {
		return false
	}
	return true
}

// Accounting claims use the existing ProcessingToken and persisted deadline.
// The log may already say completed because wallet callbacks require evidence
// before debit. It is not proof that the outbox or wallet has finished.
func withImageAccountingClaim(ctx context.Context, info *AsyncUsageInfo, write func(*gorm.DB, *AsyncUsageInfo, *Log) error) error {
	if info == nil || info.ID == 0 || info.ProcessingToken == "" || info.LogID <= 0 || !requiresAsyncUsageLogID(info) || LogDB == nil {
		return ErrImagePollClaimLost
	}
	clock, err := asyncUsageLeaseClockSQL(LogDB)
	if err != nil {
		return err
	}
	return LogDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Image tasks always lock task -> usage -> log. Synchronous measured images
		// have no task row and take usage -> log; never manufacture a task identity.
		var task ImageTask
		if info.ImageTaskID != "" {
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", info.ImageTaskID).First(&task).Error; err != nil {
				return err
			}
			if task.PrepaymentQuoteJSON != "" {
				return ErrImagePollClaimLost
			}
			if task.GroupID != info.GroupID || task.TokenID != info.TokenID || (task.UsageID != 0 && task.UsageID != info.ID) || (info.BillingOperationID != "" && task.BillingOperationID != info.BillingOperationID) {
				return ErrImagePollClaimLost
			}
		}
		var current AsyncUsageInfo
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND status=? AND processing_token=?", info.ID, AsyncUsageStatusPending, info.ProcessingToken).First(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrImagePollClaimLost
			}
			return err
		}
		if current.MeasuredImage != info.MeasuredImage ||
			current.UsageModelID != info.UsageModelID ||
			current.UsageSource != info.UsageSource ||
			current.UsageAtMs != info.UsageAtMs ||
			current.RequestAt.UnixMilli() != info.RequestAt.UnixMilli() ||
			current.ImageTaskID != info.ImageTaskID ||
			current.LogID != info.LogID ||
			current.GroupID != info.GroupID ||
			current.TokenID != info.TokenID ||
			current.TokenName != info.TokenName ||
			current.RequestID != info.RequestID ||
			current.BillingOperationID != info.BillingOperationID ||
			current.Mode != info.Mode ||
			current.ChannelID != info.ChannelID ||
			current.Model != info.Model ||
			current.InternalImageTask != info.InternalImageTask ||
			current.PricingCurrency != info.PricingCurrency ||
			current.PricingVersion != info.PricingVersion ||
			(!current.InternalImageTask && !reflect.DeepEqual(current.Price, info.Price)) {
			return ErrImagePollClaimLost
		}
		if (current.MeasuredImage || current.Amount.UsedAmount > 0) && (!reflect.DeepEqual(current.Amount, info.Amount) || !reflect.DeepEqual(current.Usage, info.Usage) || !reflect.DeepEqual(current.UsageContext, info.UsageContext)) {
			return ErrImagePollClaimLost
		}
		var now int64
		if err := tx.Raw("SELECT " + clock).Scan(&now).Error; err != nil {
			return err
		}
		deadline := current.NextPollAt.UnixMilli()
		if now <= 0 || current.NextPollAt.IsZero() || deadline <= now {
			return ErrImagePollClaimLost
		}
		// P2 identities never fall back to a legacy request-id receipt.
		if _, p2 := current.UsageIdentity(); p2 && current.BillingOperationID == "" {
			return ErrImagePollClaimLost
		}
		var entry Log
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", current.LogID).First(&entry).Error; err != nil {
			return err
		}
		if entry.GroupID != current.GroupID || (current.TokenID != 0 && entry.TokenID != current.TokenID) || (current.Mode != 0 && entry.Mode != current.Mode) || (current.BillingOperationID != "" && string(entry.BillingOperationID) != current.BillingOperationID) || (current.BillingOperationID == "" && (entry.BillingOperationID != "" || string(entry.RequestID) != current.RequestID)) || (entry.AsyncUsageStatus != AsyncUsageStatusPending && entry.AsyncUsageStatus != AsyncUsageStatusCompleted) {
			return ErrImagePollClaimLost
		}
		// Locking the final receipt can itself consume the lease. Check again before
		// writing, and again after every write using the captured pre-release deadline.
		if err := tx.Raw("SELECT " + clock).Scan(&now).Error; err != nil {
			return err
		}
		if deadline <= now {
			return ErrImagePollClaimLost
		}
		if err := write(tx, &current, &entry); err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&AsyncUsageInfo{}).Where("id=? AND status=? AND processing_token=?", current.ID, current.Status, current.ProcessingToken).Where("? > "+clock, deadline).Count(&count).Error; err != nil {
			return err
		}
		if count != 1 {
			return ErrImagePollClaimLost
		}
		return nil
	}, claimTxOptions(LogDB)...)
}

func ValidateImageAccountingClaim(ctx context.Context, info *AsyncUsageInfo) error {
	return withImageAccountingClaim(ctx, info, func(*gorm.DB, *AsyncUsageInfo, *Log) error { return nil })
}

func writeImageAccountingUsage(tx *gorm.DB, current *AsyncUsageInfo, values map[string]any) error {
	values["updated_at"] = time.Now().UTC().Truncate(time.Millisecond)
	result := tx.Model(&AsyncUsageInfo{}).Where("id=? AND status=? AND processing_token=?", current.ID, AsyncUsageStatusPending, current.ProcessingToken).Updates(values)
	if result.Error != nil {
		return result.Error
	}
	if err := verifyAccountingWrite(tx.Model(&AsyncUsageInfo{}).Where("id=?", current.ID), result.RowsAffected, values); err != nil {
		return err
	}
	if status, ok := values["status"]; ok {
		switch v := status.(type) {
		case AsyncUsageStatus:
			current.Status = v
		case int:
			current.Status = AsyncUsageStatus(v)
		}
	}
	if token, ok := values["processing_token"].(string); ok {
		current.ProcessingToken = token
	}
	return nil
}
func writeImageAccountingLog(tx *gorm.DB, entry *Log, values map[string]any) error {
	result := tx.Model(&Log{}).Where("id=?", entry.ID).Updates(values)
	if result.Error != nil {
		return result.Error
	}
	return verifyAccountingWrite(tx.Model(&Log{}).Where("id=?", entry.ID), result.RowsAffected, values)
}

// MySQL normally reports changed rows, so an idempotent unchanged receipt can
// report zero. The row is already locked: accept only its exact written fields,
// never a missing row or an update suppressed without the expected values.
func verifyAccountingWrite(query *gorm.DB, affected int64, values map[string]any) error {
	if affected == 1 {
		return nil
	}
	if affected != 0 {
		return ErrImagePollClaimLost
	}
	var count int64
	if err := query.Where(values).Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return ErrImagePollClaimLost
	}
	return nil
}
func updateImageAccountingUsage(info *AsyncUsageInfo, values map[string]any) error {
	return withImageAccountingClaim(context.Background(), info, func(tx *gorm.DB, current *AsyncUsageInfo, _ *Log) error {
		return writeImageAccountingUsage(tx, current, values)
	})
}
