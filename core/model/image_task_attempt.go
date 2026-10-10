package model

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/labring/aiproxy/core/common/failover"
	"gorm.io/gorm"
)

type ImageTaskAttempt struct {
	Retry      bool             `json:"retry"`
	ChannelID  int              `json:"channel_id"`
	StartedAt  time.Time        `json:"started_at"`
	DurationMS int64            `json:"duration_ms"`
	Failure    failover.Failure `json:"failure"`
	Decision   string           `json:"decision"`
	// Unknown/accepted attempts may incur procurement cost even if no customer charge posts.
	PotentialCost string `json:"potential_cost"`
}

// SaveImageTaskAttempt persists the audit and route before any upstream side effect.
// expected is a compare-and-swap guard against concurrent dispatch/replay.
func SaveImageTaskAttempt(task *ImageTask, expected int, channelID int, baseURL string) error {
	raw, err := json.Marshal(task.Attempts)
	if err != nil {
		return err
	}
	return LogDB.Transaction(func(tx *gorm.DB) error {
		var current ImageTask
		if err := tx.First(&current, "id = ?", task.ID).Error; err != nil {
			return err
		}
		if current.Status != "submitting" || len(current.Attempts) != expected {
			return errors.New("image attempt reservation changed")
		}
		if len(task.Attempts) != expected && len(task.Attempts) != expected+1 {
			return errors.New("invalid attempt transition")
		}
		if len(task.Attempts) == expected+1 && expected > 0 {
			last := current.Attempts[expected-1]
			if last.Failure.Acceptance != failover.NotAccepted || !last.Retry {
				return errors.New("previous image attempt cannot be retried")
			}
		}
		previous, err := json.Marshal(current.Attempts)
		if err != nil {
			return err
		}
		q := tx.Model(&ImageTask{}).Where("id = ? AND status = ?", task.ID, "submitting")
		if expected == 0 {
			q = q.Where("attempts IS NULL OR attempts = ? OR attempts = ?", "null", "[]")
		} else {
			q = q.Where("attempts = ?", string(previous))
		}
		result := q.Updates(map[string]any{"attempts": string(raw), "validation_contract": task.ValidationContract, "upstream_model": task.UpstreamModel, "channel_type": task.ChannelType, "key_fingerprint": task.KeyFingerprint, "expected_images": task.ExpectedImages, "updated_at": time.Now()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("image attempt already claimed")
		}
		if err := tx.Model(&AsyncUsageInfo{}).Where("id = ? AND status = ?", task.UsageID, AsyncUsageStatusNone).Updates(map[string]any{"channel_id": channelID, "base_url": baseURL}).Error; err != nil {
			return err
		}
		var entry Log
		if err := tx.Where("id IN (?)", tx.Model(&AsyncUsageInfo{}).Select("log_id").Where("id = ?", task.UsageID)).First(&entry).Error; err != nil {
			return err
		}
		if entry.Metadata == nil {
			entry.Metadata = map[string]string{}
		}
		audit := make([]map[string]any, 0, len(task.Attempts))
		for _, attempt := range task.Attempts {
			audit = append(audit, map[string]any{"channel_id": attempt.ChannelID, "acceptance": attempt.Failure.Acceptance, "failure_class": attempt.Failure.Class, "evidence": attempt.Failure.Evidence, "duration_ms": attempt.DurationMS, "decision": attempt.Decision, "retry": attempt.Retry})
		}
		encoded, err := json.Marshal(audit)
		if err != nil {
			return err
		}
		entry.Metadata["channel_failover_attempts"] = string(encoded)
		entry.ChannelID = channelID
		return tx.Save(&entry).Error
	})
}
