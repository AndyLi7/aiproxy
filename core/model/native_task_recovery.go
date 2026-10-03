package model

import (
	"encoding/json"
	"gorm.io/gorm"
	"time"
)

// ClaimNativeRecovery leases one bounded page. It never returns a new submission
// to be sent to a provider: unknown outcomes are reconciliation-only.
func ClaimNativeRecovery(db *gorm.DB, owner string, now time.Time, limit int) ([]NativeTask, error) {
	if owner == "" || len(owner) > 64 || limit < 1 || limit > 100 {
		return nil, ErrNativeTaskConflict
	}
	var candidates []NativeTask
	err := db.Where("next_recovery_at <= ? AND recovery_until <= ? AND (status NOT IN ? OR billing_settled = ?)", now.Unix(), now.Unix(), []string{"completed", "failed"}, false).Order("next_recovery_at ASC, created_at ASC").Limit(limit).Find(&candidates).Error
	if err != nil {
		return nil, err
	}
	claimed := []NativeTask{}
	for _, task := range candidates {
		result := db.Model(&NativeTask{}).Where("id = ? AND recovery_until <= ? AND next_recovery_at <= ?", task.ID, now.Unix(), now.Unix()).Updates(map[string]any{"recovery_owner": owner, "recovery_until": now.Add(2 * time.Minute).Unix()})
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 1 {
			task.RecoveryOwner = owner
			claimed = append(claimed, task)
		}
	}
	return claimed, nil
}
func ReleaseNativeRecovery(db *gorm.DB, id, owner string, next time.Time) error {
	result := db.Model(&NativeTask{}).Where("id = ? AND recovery_owner = ?", id, owner).Updates(map[string]any{"recovery_owner": "", "recovery_until": 0, "next_recovery_at": next.Unix()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrNativeTaskConflict
	}
	return nil
}
func SaveNativeBillingTerminal(db *gorm.DB, id, group string, token int, receipt string) error {
	var parsed struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(receipt), &parsed) != nil {
		return ErrNativeTaskConflict
	}
	terminal := parsed.Status == "settled" || parsed.Status == "refunded" || parsed.Status == "estimated"
	return db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&NativeTask{}).Where("id = ? AND group_id = ? AND token_id = ?", id, group, token).Update("billing_settled", terminal)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrNativeTaskConflict
		}
		if parsed.Status == "refunded" {
			if err := tx.Model(&NativeTask{}).Where("id = ? AND group_id = ? AND token_id = ? AND status IN ?", id, group, token, []string{"reserved", "submitting", "submission_unknown"}).Updates(map[string]any{"status": "failed", "error_code": "submission_timeout"}).Error; err != nil {
				return err
			}
		}
		return syncNativeTaskLog(tx, id, group, token, receipt)
	})
}
