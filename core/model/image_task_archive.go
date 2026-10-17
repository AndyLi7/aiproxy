package model

import (
	"encoding/json"
	"errors"
	"gorm.io/gorm"
	"time"
)

// SaveArchivedImage checkpoints each successful upload; retrying storage never
// returns to provider submission or discards already archived outputs.
func SaveArchivedImage(task *ImageTask, index int, output ImageOutput) error {
	return saveArchivedImage(LogDB, task, index, output)
}
func saveArchivedImage(db *gorm.DB, task *ImageTask, index int, output ImageOutput) error {
	if task.Status != "result_processing" || index < 0 || index >= len(task.Data) || !output.Stored || !ValidAuxiliaryImages(output) {
		return errors.New("invalid archive checkpoint")
	}
	before, err := json.Marshal(task.Data)
	if err != nil {
		return err
	}
	next := CloneImageOutputs(task.Data)
	next[index] = output
	after, err := json.Marshal(next)
	if err != nil {
		return err
	}
	result := db.Model(&ImageTask{}).Where("id = ? AND status = ? AND data = ?", task.ID, "result_processing", string(before)).Updates(map[string]any{"data": string(after), "updated_at": time.Now().UTC()})
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
	return completeImageArchive(LogDB, task)
}
func completeImageArchive(db *gorm.DB, task *ImageTask) error {
	if len(task.Data) == 0 {
		return errors.New("empty archive")
	}
	for _, out := range task.Data {
		if !out.Stored || !ValidAuxiliaryImages(out) {
			return errors.New("incomplete archive")
		}
		for _, asset := range out.AuxiliaryImages {
			if asset != nil && !asset.Stored {
				return errors.New("incomplete auxiliary archive")
			}
		}
	}
	encoded, err := json.Marshal(task.Data)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	result := db.Model(&ImageTask{}).Where("id = ? AND status = ? AND data = ?", task.ID, "result_processing", string(encoded)).Updates(map[string]any{
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
