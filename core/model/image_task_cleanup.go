package model

import (
	"encoding/json"
	"errors"
	"time"
)

// Only newly scoped generated-result objects are eligible. Legacy content-hash
// objects may be shared with permanent examples and must not be deleted here.
func ListExpiredTemporaryImageTasks(now time.Time, afterID string, limit int) ([]ImageTask, error) {
	var tasks []ImageTask
	query := LogDB.Where("status = ? AND archive_required = ? AND result_expires_at <= ? AND data LIKE ?", "completed", true, now, "%generated-results/images/%")
	if afterID != "" {
		query = query.Where("id > ?", afterID)
	}
	err := query.Order("id ASC").Limit(limit).Find(&tasks).Error
	return tasks, err
}

// Checkpoint deletion after the object store confirms success. Retain task,
// usage, and billing records, but remove dead media URLs from the task row.
func MarkExpiredImageMediaDeleted(task *ImageTask, outputs []ImageOutput, now time.Time) error {
	if task.Status != "completed" || task.ResultExpiresAt == nil || now.Before(*task.ResultExpiresAt) || len(outputs) != len(task.Data) {
		return errors.New("invalid expired image cleanup")
	}
	before, err := json.Marshal(task.Data)
	if err != nil {
		return err
	}
	after, err := json.Marshal(outputs)
	if err != nil {
		return err
	}
	result := LogDB.Model(&ImageTask{}).
		Where("id = ? AND status = ? AND data = ?", task.ID, "completed", string(before)).
		Updates(map[string]any{"data": string(after), "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("expired image cleanup changed concurrently")
	}
	task.Data = outputs
	return nil
}
