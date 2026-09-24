package model

import (
	"encoding/json"
	"errors"
	"time"
)

// SaveArchivedImage checkpoints each successful upload; retrying storage never
// returns to provider submission or discards already archived outputs.
func SaveArchivedImage(task *ImageTask, index int, output ImageOutput) error {
	if task.Status != "result_processing" || index < 0 || index >= len(task.Data) || !output.Stored {
		return errors.New("invalid archive checkpoint")
	}
	before, err := json.Marshal(task.Data)
	if err != nil {
		return err
	}
	next := append([]ImageOutput(nil), task.Data...)
	next[index] = output
	after, err := json.Marshal(next)
	if err != nil {
		return err
	}
	result := LogDB.Model(&ImageTask{}).Where("id = ? AND status = ? AND data = ?", task.ID, "result_processing", string(before)).Updates(map[string]any{"data": string(after), "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("archive checkpoint changed")
	}
	task.Data = next
	return nil
}

func CompleteImageArchive(task *ImageTask) error {
	if len(task.Data) == 0 {
		return errors.New("empty archive")
	}
	for _, out := range task.Data {
		if !out.Stored {
			return errors.New("incomplete archive")
		}
	}
	now := time.Now().UTC()
	result := LogDB.Model(&ImageTask{}).Where("id = ? AND status = ?", task.ID, "result_processing").Updates(map[string]any{
		"status": "completed", "completed_at": now, "updated_at": now, "result_expires_at": now.Add(7 * 24 * time.Hour), "retain_until": now.Add(30 * 24 * time.Hour),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("archive state changed")
	}
	return nil
}
