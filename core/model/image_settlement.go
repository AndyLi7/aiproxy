package model

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// CreateMeasuredImageSettlement commits the audit snapshot and outbox atomically.
// No external debit can start before this transaction succeeds. Pending evidence
// is immutable; a billing worker must never repeat inference to repair it.
func CreateMeasuredImageSettlement(entry *Log, info *AsyncUsageInfo) error {
	if entry == nil || info == nil || info.Amount.ImageBillingResult == nil {
		return errors.New("missing measured image settlement")
	}

	info.MeasuredImage = true

	info.Status = AsyncUsageStatusPending
	switch info.Amount.ImageBillingResult.State {
	case "pending":
		info.Status = AsyncUsageStatusMeasurementPending
	case "failed":
		info.Status = AsyncUsageStatusFailed
	case "complete":
		if info.Amount.UsedAmount == 0 {
			info.Status = AsyncUsageStatusCompleted
		}
	default:
		return errors.New("invalid measured settlement state")
	}

	entry.AsyncUsageStatus = info.Status
	entry.Price = info.Price
	entry.Usage = info.Usage
	entry.UsageContext = info.UsageContext
	entry.Amount = info.Amount
	info.NextPollAt = time.Now().Add(AsyncUsageDefaultPollDelay)

	return LogDB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(entry).Error; err != nil {
			return err
		}

		info.LogID = entry.ID

		return tx.Create(info).Error
	})
}

func ParkMeasuredImageUsage(info *AsyncUsageInfo) error {
	return LogDB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&AsyncUsageInfo{}).
			Where("id = ? AND processing_token = ?", info.ID, info.ProcessingToken).
			Updates(map[string]any{"status": AsyncUsageStatusMeasurementPending, "processing_token": "", "error": "immutable image measurement pending"})
		if result.Error != nil {
			return result.Error
		}

		if result.RowsAffected != 1 {
			return errors.New("measured usage claim lost")
		}

		return tx.Model(&Log{}).
			Where("id = ?", info.LogID).
			Update("async_usage_status", AsyncUsageStatusMeasurementPending).
			Error
	})
}

// PrepareClaimedImageTaskMeasurement freezes both the outbox and audit evidence
// under the accounting lease. The next worker can settle without polling again.
func PrepareClaimedImageTaskMeasurement(
	info *AsyncUsageInfo,
	usage Usage,
	usageContext UsageContext,
	amount Amount,
) error {
	if info == nil || info.ProcessingToken == "" || info.LogID == 0 ||
		amount.ImageBillingResult == nil {
		return errors.New("missing image task measurement log")
	}

	update := &AsyncUsageInfo{
		Usage:         usage,
		UsageContext:  usageContext,
		Amount:        amount,
		MeasuredImage: true,
	}

	values, err := asyncUsageUpdateValues(
		update,
		"Usage",
		"UsageContext",
		"Amount",
		"MeasuredImage",
	)
	if err != nil {
		return err
	}

	return LogDB.Transaction(func(tx *gorm.DB) error {
		row := tx.Model(&AsyncUsageInfo{}).
			Where("id = ? AND processing_token = ? AND status = ?", info.ID, info.ProcessingToken, AsyncUsageStatusPending).
			Updates(values)
		if row.Error != nil {
			return row.Error
		}

		if row.RowsAffected != 1 {
			return errors.New("image measurement claim lost")
		}

		logValues, err := asyncUsageUpdateValues(update, "Usage", "UsageContext", "Amount")
		if err != nil {
			return err
		}

		row = tx.Model(&Log{}).Where("id = ?", info.LogID).Updates(logValues)
		if row.Error != nil {
			return row.Error
		}

		if row.RowsAffected != 1 {
			return errors.New("image measurement log missing")
		}

		return nil
	})
}
