package model

import (
	"gorm.io/gorm"
	"time"
)

// ShortenImageRecoveryDeadlineForTest shortens only an isolated test fixture's
// original lease and capability together. It is absent from production builds;
// real claims always receive two minutes from the locked database clock.
func ShortenImageRecoveryDeadlineForTest(task *ImageTask, deadline time.Time) error {
	h := task.imageRecoveryClaim
	if h == nil || deadline.UnixMicro() >= h.row.UntilMicros {
		return ErrChannelAdmissionConflict
	}
	deadline = deadline.UTC().Truncate(time.Microsecond)
	err := h.store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&ChannelAdmissionImageRecoveryClaim{}).Where("id = ? AND owner = ?", h.row.ID, h.row.Owner).Update("until_micros", deadline.UnixMicro()).Error; err != nil {
			return err
		}
		return tx.Model(&ImageTask{}).Where("id = ?", task.ID).Update("billing_next_check_at", deadline).Error
	})
	if err == nil {
		h.row.UntilMicros = deadline.UnixMicro()
	}
	return err
}
