package model

import (
	"context"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"sync/atomic"
	"time"
)

// ChannelAdmissionImageRecoveryClaim is only current business-lease ownership.
// It is NOT an executor/exit journal and never proves continuous coverage.
// UntilMicros mirrors the existing two-minute billing check claim exactly.
type ChannelAdmissionImageRecoveryClaim struct {
	ID                string `gorm:"primaryKey;size:64"`
	Version           int    `gorm:"not null"`
	Owner             string `gorm:"size:64;not null"`
	UntilMicros       int64  `gorm:"not null"`
	StorageGeneration string `gorm:"size:128;not null"`
	UseID             string `gorm:"size:64;not null"`
	TaskHash          string `gorm:"size:64;not null"`
}

type imageBillingRecoveryClaim struct {
	store           *ChannelAdmissionStore
	row             ChannelAdmissionImageRecoveryClaim
	handoffPrepared atomic.Bool
}

func imageRecoveryTaskHash(t *ImageTask) string {
	hash, _ := admissionHash([]any{"image-billing-recovery-v1", t.ID, t.GroupID, t.TokenID, t.CreatedAt.UnixMicro(), t.UsageID, t.Fingerprint, t.BillingOperationID, t.PrepaymentQuoteJSON, t.Model, t.ChannelType, t.KeyFingerprint, t.UpstreamModel, t.UpstreamID})
	return hash
}

func imageRecoveryClaimStoragePresent(tx *gorm.DB) (bool, error) {
	var count int64
	var err error
	switch tx.Dialector.Name() {
	case "sqlite":
		err = tx.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", "channel_admission_image_recovery_claims").Scan(&count).Error
	case "postgres":
		err = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=?", "channel_admission_image_recovery_claims").Scan(&count).Error
	default:
		return false, ErrChannelAdmissionUnavailable
	}
	if err != nil {
		return false, err
	}
	base, err := channelExecutionStoragePresent(tx)
	if err != nil {
		return false, err
	}
	if !base {
		if count != 0 {
			return false, ErrChannelAdmissionUnavailable
		}
		return false, nil
	}
	var protocols []ChannelAdmissionExecutionProtocol
	if err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", 5).Find(&protocols).Error; err != nil {
		return false, err
	}
	if count == 0 && len(protocols) == 0 {
		return false, nil
	}
	if count != 1 || len(protocols) != 1 || protocols[0].Version != 1 {
		return false, ErrChannelAdmissionUnavailable
	}
	statement := &gorm.Statement{DB: tx}
	if err = statement.Parse(&ChannelAdmissionImageRecoveryClaim{}); err != nil {
		return false, err
	}
	rows, err := tx.Model(&ChannelAdmissionImageRecoveryClaim{}).Select(statement.Schema.DBNames).Limit(0).Rows()
	if err != nil {
		return false, err
	}
	rowErr := rows.Err()
	closeErr := rows.Close()
	if rowErr != nil {
		return false, rowErr
	}
	return true, closeErr
}

// ClaimImagePrepaymentRecovery keeps the existing two-minute scheduling claim.
// The optional sidecar makes ownership unique even if the same timestamps are
// reused. The private capability is attached only AFTER the transaction commits.
func ClaimImagePrepaymentRecovery(ctx context.Context, task *ImageTask, now time.Time) (bool, error) {
	if task == nil || task.imageRecoveryClaim != nil {
		return false, ErrChannelAdmissionConflict
	}
	now = now.UTC()
	until := now.Add(2 * time.Minute)
	legacy := func(db *gorm.DB) (bool, error) {
		result := db.WithContext(ctx).Model(&ImageTask{}).Where("id = ? AND (billing_next_check_at IS NULL OR billing_next_check_at <= ?)", task.ID, now).Update("billing_next_check_at", until)
		return result.RowsAffected == 1, result.Error
	}
	store, err := channelAdmissionRuntimeStore(LogDB)
	if err != nil {
		return false, err
	}
	if store == nil {
		return legacy(LogDB)
	}
	present, err := imageRecoveryClaimStoragePresent(LogDB)
	if err != nil {
		return false, err
	}
	if !present {
		return legacy(LogDB)
	}
	owner, err := newAdmissionGeneration()
	if err != nil {
		return false, err
	}
	var handle *imageBillingRecoveryClaim
	claimed := false
	err = store.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		present, e := imageRecoveryClaimStoragePresent(tx)
		if e != nil {
			return e
		}
		if !present {
			return ErrChannelAdmissionUnavailable
		}
		inactive, e := channelAdmissionNeverActivated(tx, c)
		if e != nil {
			return e
		}
		if inactive {
			claimed, e = legacy(tx)
			return e
		}
		use, e := imageReservationUse(tx, task)
		if e != nil {
			return e
		}
		generation := ""
		if use != nil {
			generation = use.ID
		}
		if task.AdmissionGeneration != "" && task.AdmissionGeneration != generation {
			return ErrChannelAdmissionConflict
		}
		snapshot := *task
		snapshot.AdmissionGeneration = generation
		current, e := lockAdmissionImageTask(tx, &snapshot)
		if e != nil {
			return e
		}
		if imageRecoveryTaskHash(current) != imageRecoveryTaskHash(task) {
			return ErrChannelAdmissionConflict
		}
		if e = sameImageWalletSnapshot(current, task); e != nil {
			return e
		}
		id := imageAdmissionResource(current)
		var old []ChannelAdmissionImageRecoveryClaim
		if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Find(&old).Error; e != nil {
			return e
		}
		// Only after BOTH task and sidecar locks: a slow earlier item or a
		// lock wait must not reuse the batch timestamp as a fresh lease.
		clock, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		if current.PrepaymentQuoteJSON == "" || current.BillingSettled || (current.BillingNextCheckAt != nil && current.BillingNextCheckAt.After(clock)) {
			return nil
		}
		until := clock.UTC().Add(2 * time.Minute).Truncate(time.Microsecond)
		if len(old) > 0 {
			previous := old[0]
			if previous.Version != 1 || previous.StorageGeneration != c.StorageGeneration || !fullIdentityDigest(previous.TaskHash) ||
				(previous.Owner == "" && previous.UntilMicros != 0) || (previous.Owner != "" && (!fullIdentityDigest(previous.Owner) || previous.UntilMicros <= 0)) {
				return ErrChannelAdmissionConflict
			}
			// A stale scheduling timestamp cannot steal a still-live unique claim.
			if previous.Owner != "" && previous.UntilMicros > clock.UnixMicro() {
				return nil
			}
		}
		row := ChannelAdmissionImageRecoveryClaim{ID: id, Version: 1, Owner: owner, UntilMicros: until.UnixMicro(), StorageGeneration: c.StorageGeneration, UseID: generation, TaskHash: imageRecoveryTaskHash(current)}
		if e = tx.Save(&row).Error; e != nil {
			return e
		}
		result := tx.Model(&ImageTask{}).Where("id = ? AND group_id = ? AND token_id = ?", task.ID, task.GroupID, task.TokenID).Update("billing_next_check_at", until)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrChannelAdmissionConflict
		}
		if e = checkImageRecoveryClaim(tx, c, current, &row, false); e != nil {
			return e
		}
		claimed = true
		handle = &imageBillingRecoveryClaim{store: store, row: row}
		return nil
	}, true)
	if err != nil {
		return false, err
	}
	if handle != nil {
		task.imageRecoveryClaim = handle
		task.AdmissionGeneration = handle.row.UseID
	}
	return claimed, nil
}

func checkImageRecoveryClaim(tx *gorm.DB, c *ChannelAdmissionControl, task *ImageTask, expected *ChannelAdmissionImageRecoveryClaim, released bool) error {
	if expected.StorageGeneration != c.StorageGeneration || imageRecoveryTaskHash(task) != expected.TaskHash {
		return ErrChannelAdmissionConflict
	}
	owner, until := expected.Owner, expected.UntilMicros
	if released {
		owner = ""
		until = 0
	}
	var count int64
	err := tx.Model(&ChannelAdmissionImageRecoveryClaim{}).Where("id = ? AND version = ? AND owner = ? AND until_micros = ? AND storage_generation = ? AND use_id = ? AND task_hash = ?", expected.ID, 1, owner, until, expected.StorageGeneration, expected.UseID, expected.TaskHash).
		Where("? > "+admissionClockSQL(tx), expected.UntilMicros/1000).Count(&count).Error
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrChannelAdmissionConflict
	}
	return nil
}

func (h *imageBillingRecoveryClaim) withClaim(ctx context.Context, task *ImageTask, release bool, work func(*gorm.DB, *ImageTask) error) error {
	return h.withClaimControl(ctx, task, release, func(tx *gorm.DB, c *ChannelAdmissionControl, current *ImageTask) error { return work(tx, current) })
}
func (h *imageBillingRecoveryClaim) withClaimControl(ctx context.Context, task *ImageTask, release bool, work func(*gorm.DB, *ChannelAdmissionControl, *ImageTask) error) error {

	if h == nil || h.store == nil || h.row.Owner == "" || task == nil {
		return ErrChannelAdmissionConflict
	}
	return h.store.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		present, err := imageRecoveryClaimStoragePresent(tx)
		if err != nil {
			return err
		}
		if !present {
			return ErrChannelAdmissionUnavailable
		}
		snapshot := *task
		snapshot.AdmissionGeneration = h.row.UseID
		current, err := lockAdmissionImageTask(tx, &snapshot)
		if err != nil {
			return err
		}
		var row ChannelAdmissionImageRecoveryClaim
		if err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", h.row.ID).Error; err != nil {
			return err
		}
		if err = checkImageRecoveryClaim(tx, c, current, &h.row, false); err != nil {
			return err
		}
		if err = work(tx, c, current); err != nil {
			return err
		}
		return checkImageRecoveryClaim(tx, c, current, &h.row, release)
	}, true)
}

// ValidateImagePrepaymentRecovery runs before every wallet call made by this
// recovery claim. It does not grant new submission or certify remote completion.
func ValidateImagePrepaymentRecovery(ctx context.Context, task *ImageTask) error {
	if task == nil {
		return ErrChannelAdmissionConflict
	}
	if task.imageRecoveryClaim == nil {
		return nil
	}
	return task.imageRecoveryClaim.withClaim(ctx, task, false, func(tx *gorm.DB, current *ImageTask) error { return sameImageWalletSnapshot(current, task) })
}

// ReleaseImagePrepaymentRecovery is invoked only after the owned Sync call and
// cleanup normally returned. It clears current ownership, not executor/use
// evidence. No capability is cleared in memory: a stale task cannot downgrade
// later wallet calls or cleanup to the legacy path.
func ReleaseImagePrepaymentRecovery(ctx context.Context, task *ImageTask) error {
	if task != nil && task.imageRecoveryClaim != nil && task.imageRecoveryClaim.handoffPrepared.Load() {
		return ErrChannelAdmissionConflict
	}
	if task == nil {
		return ErrChannelAdmissionConflict
	}
	if task.imageRecoveryClaim == nil {
		return nil
	}
	return task.imageRecoveryClaim.withClaim(ctx, task, true, func(tx *gorm.DB, current *ImageTask) error {
		// A committed offer with a lost response still forbids legacy release.
		present, err := imageRecoveryHandoffStoragePresent(tx)
		if err != nil {
			return err
		}
		if present {
			var count int64
			if err = tx.Model(&ChannelAdmissionImageRecoveryHandoff{}).Where("claim_id = ? AND claim_owner = ?", task.imageRecoveryClaim.row.ID, task.imageRecoveryClaim.row.Owner).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return ErrChannelAdmissionConflict
			}
		}
		return releaseImageRecoveryClaim(tx, task.imageRecoveryClaim.row)
	})
}

func releaseImageRecoveryClaim(tx *gorm.DB, row ChannelAdmissionImageRecoveryClaim) error {
	result := tx.Model(&ChannelAdmissionImageRecoveryClaim{}).Where("id = ? AND owner = ? AND until_micros = ?", row.ID, row.Owner, row.UntilMicros).Updates(map[string]any{"owner": "", "until_micros": 0})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrChannelAdmissionConflict
	}
	return nil
}
