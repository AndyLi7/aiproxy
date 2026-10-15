package model

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/labring/aiproxy/core/common/failover"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func imageAdmissionResource(task *ImageTask) string {
	hash, _ := admissionHash([]any{"image-reservation-v1", task.ID, task.GroupID, task.TokenID})
	return hash
}
func imageAdmissionRequestHash(task *ImageTask) string {
	hash, _ := admissionHash([]any{"image-request-v1", task.Fingerprint, task.BillingOperationID})
	return hash
}
func imageAttemptResource(task *ImageTask, index int) string {
	hash, _ := admissionHash([]any{"image-attempt-v1", imageAdmissionResource(task), task.AdmissionGeneration, index})
	return hash
}
func imageAttemptHash(task *ImageTask, index int, identity ChannelIdentity) string {
	hash, _ := admissionHash([]any{"image-attempt-v1", imageAdmissionRequestHash(task), task.AdmissionGeneration, index, identity})
	return hash
}
func imageReleaseHash(use ChannelAdmissionUse) string {
	hash, _ := admissionHash([]any{"image-unadmitted-release-v1", use.ID, use.ResourceKey, use.RequestHash, use.IdentityHash})
	return hash
}
func imageReservationUse(tx *gorm.DB, task *ImageTask) (*ChannelAdmissionUse, error) {
	var uses []ChannelAdmissionUse
	if err := tx.Where("resource_key = ?", imageAdmissionResource(task)).Find(&uses).Error; err != nil {
		return nil, err
	}
	var active *ChannelAdmissionUse
	for _, use := range uses {
		if use.Lane != "image" {
			return nil, ErrChannelAdmissionConflict
		}
		if use.State == "closed" && use.ClosureHash == imageReleaseHash(use) && use.ClosedAtMs > 0 {
			continue
		}
		if active != nil || use.State != "open" || use.RequestHash != imageAdmissionRequestHash(task) || use.ClosedAtMs != 0 || use.ClosureHash != "" {
			return nil, ErrChannelAdmissionConflict
		}
		copy := use
		active = &copy
	}
	if active == nil && len(uses) > 0 {
		return nil, ErrChannelAdmissionConflict
	}
	return active, nil
}
func lockAdmissionImageTask(tx *gorm.DB, task *ImageTask) (*ImageTask, error) {
	var current ImageTask
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND group_id = ? AND token_id = ?", task.ID, task.GroupID, task.TokenID).First(&current).Error
	if err != nil {
		return nil, err
	}
	if !current.CreatedAt.Equal(task.CreatedAt) || current.Fingerprint != task.Fingerprint || current.BillingOperationID != task.BillingOperationID || current.UsageID != task.UsageID {
		return nil, ErrChannelAdmissionConflict
	}
	use, err := imageReservationUse(tx, &current)
	if err != nil {
		return nil, err
	}
	generation := ""
	if use != nil {
		generation = use.ID
	}
	if task.AdmissionGeneration != generation {
		return nil, ErrChannelAdmissionConflict
	}
	return &current, nil
}

func reserveImageTaskWithAdmission(task *ImageTask, info *AsyncUsageInfo, operational ...OperationalFields) (saved *ImageTask, created bool, err error) {
	store, err := channelAdmissionRuntimeStore(LogDB)
	if err != nil {
		return nil, false, err
	}
	if store == nil {
		return reserveImageTask(LogDB, task, info, operational...)
	}
	var count int64
	if err = LogDB.Model(&ImageTask{}).Where("id = ?", task.ID).Count(&count).Error; err != nil {
		return nil, false, err
	}
	if count > 0 {
		store.LoadDeployment = nil
	}
	generation, err := newAdmissionGeneration()
	if err != nil {
		return nil, false, err
	}
	input := ChannelAdmissionRequest{RequestID: generation, ResourceKey: imageAdmissionResource(task), GroupID: task.GroupID, TokenID: task.TokenID, Lane: "image", RequestHash: imageAdmissionRequestHash(task), Identity: ChannelIdentity{ChannelID: info.ChannelID, KeyDigest: task.KeyFingerprint, IdentityHash: task.AdmissionIdentityHash}}
	protected := false
	reserve := func(tx *gorm.DB) error {
		var e error
		saved, created, e = reserveImageTask(tx, task, info, operational...)
		if e == nil && created && protected {
			now, clockErr := channelAdmissionTime(tx)
			if clockErr != nil {
				return clockErr
			}
			saved.CreatedAt = now
			e = tx.Model(&ImageTask{}).Where("id = ?", saved.ID).UpdateColumn("created_at", now).Error
		}
		return e
	}
	use, _, err := store.admit(context.Background(), input, reserve, func(tx *gorm.DB, c *ChannelAdmissionControl) (bool, error) {
		inactive, e := channelAdmissionNeverActivated(tx, c)
		if e != nil {
			return false, e
		}
		if inactive {
			return true, reserve(tx)
		}
		var previous ImageTask
		e = tx.Select("id").Where("id = ?", task.ID).First(&previous).Error
		if e == nil {
			if e = reserve(tx); e != nil {
				return true, e
			}
			existing, e := imageReservationUse(tx, saved)
			if e != nil {
				return true, e
			}
			if existing != nil {
				saved.AdmissionGeneration = existing.ID
			}
			return true, nil
		}
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return false, e
		}
		var prior []ChannelAdmissionUse
		if e = tx.Where("resource_key = ?", input.ResourceKey).Find(&prior).Error; e != nil {
			return false, e
		}
		for _, old := range prior {
			if old.Lane != "image" || old.State != "closed" || old.ClosedAtMs <= 0 || old.ClosureHash != imageReleaseHash(old) {
				return false, ErrChannelAdmissionRetained
			}
		}
		if !fullIdentityDigest(task.Fingerprint) || task.BillingOperationID == "" {
			return false, ErrChannelAdmissionConflict
		}
		protected = true
		return false, nil
	})
	if err != nil {
		return nil, false, err
	}
	if use != nil {
		if saved == nil {
			return nil, false, ErrChannelAdmissionConflict
		}
		saved.AdmissionGeneration = use.ID
	}
	return saved, created, nil
}

func releaseImageTaskWithAdmission(id string, evidence *ImageTask) error {
	store, err := channelAdmissionRuntimeStore(LogDB)
	if err != nil {
		return err
	}
	if store == nil {
		return releaseImageTaskReservation(LogDB, id)
	}
	return store.transaction(context.Background(), func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		inactive, e := channelAdmissionNeverActivated(tx, c)
		if e != nil {
			return e
		}
		if inactive {
			return releaseImageTaskReservation(tx, id)
		}
		if evidence == nil || evidence.ID != id {
			return ErrChannelAdmissionConflict
		}
		current, e := lockAdmissionImageTask(tx, evidence)
		if e != nil {
			return e
		}
		// A wallet refusal captured before any attempt cannot race a prepared
		// attempt, even while its wallet claim is still in flight.
		var count int64
		if e = tx.Model(&ChannelAdmissionUse{}).Where("resource_key = ?", imageAttemptResource(evidence, 0)).Count(&count).Error; e != nil {
			return e
		}
		if count != 0 {
			return ErrChannelAdmissionRetained
		}
		use, e := imageReservationUse(tx, current)
		if e != nil {
			return e
		}
		if e = releaseImageTaskReservation(tx, id); e != nil {
			return e
		}
		if use == nil {
			return nil
		}
		now, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		result := tx.Model(&ChannelAdmissionUse{}).Where("id = ? AND state = ?", use.ID, "open").Updates(map[string]any{"state": "closed", "closed_at_ms": now.UnixMilli(), "closure_hash": imageReleaseHash(*use)})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrChannelAdmissionConflict
		}
		return nil
	}, true)
}

// PrepareImageTaskAttempt runs before a wallet claim. It records a unique use
// for this reservation generation and attempt index. Unknown claim outcomes
// never permit another caller to replay the attempt. Frozen first attempts
// retain their original admission; failover routes require new admission.
func PrepareImageTaskAttempt(ctx context.Context, task *ImageTask, identity ChannelIdentity, index int) (string, error) {
	store, err := channelAdmissionRuntimeStore(LogDB)
	if err != nil {
		return "", err
	}
	if store == nil {
		return "", nil
	}
	nonce, err := newAdmissionGeneration()
	if err != nil {
		return "", err
	}
	input := ChannelAdmissionRequest{RequestID: nonce, ResourceKey: imageAttemptResource(task, index), GroupID: task.GroupID, TokenID: task.TokenID, Lane: "image", RequestHash: imageAttemptHash(task, index, identity), Identity: identity}
	var frozenID string
	use, created, err := store.admit(ctx, input, func(tx *gorm.DB) error {
		current, e := lockAdmissionImageTask(tx, task)
		if e != nil {
			return e
		}
		return checkImageAdmissionSubmitTime(tx, current)
	}, func(tx *gorm.DB, c *ChannelAdmissionControl) (bool, error) {
		inactive, e := channelAdmissionNeverActivated(tx, c)
		if e != nil || inactive {
			return inactive, e
		}
		if index < 0 || !validChannelIdentity(identity) {
			return false, ErrChannelAdmissionConflict
		}
		current, e := lockAdmissionImageTask(tx, task)
		if e != nil {
			return false, e
		}
		if current.Status != "submitting" || len(current.Attempts) != index {
			return false, ErrChannelAdmissionConflict
		}
		if e = checkImageAdmissionSubmitTime(tx, current); e != nil {
			return false, e
		}
		if index > 0 {
			last := current.Attempts[index-1]
			if last.Failure.Acceptance != failover.NotAccepted || !last.Retry {
				return false, ErrChannelAdmissionConflict
			}
		}
		var count int64
		if e = tx.Model(&ChannelAdmissionUse{}).Where("resource_key = ?", input.ResourceKey).Count(&count).Error; e != nil {
			return false, e
		}
		if count != 0 {
			return false, ErrChannelAdmissionConflict
		}
		if index > 0 {
			return false, nil
		}
		// Initial work was frozen at reservation. Validate the original full
		// identity; an unrelated successor must not inherit its authorization.
		initial, e := imageReservationUse(tx, current)
		if e != nil {
			return false, e
		}
		if initial == nil {
			// Activation must not strand a reservation already frozen before
			// C. Bind its persisted route and real stored key before adopting
			// a generation; this is never permission for another reservation.
			channel, e := storedAdmissionIdentity(tx, identity)
			if e != nil {
				return false, e
			}
			var info AsyncUsageInfo
			if e = tx.First(&info, "id = ? AND image_task_id = ?", current.UsageID, current.ID).Error; e != nil {
				return false, e
			}
			if info.ChannelID != identity.ChannelID || info.BaseURL != channel.BaseURL || current.ChannelType != channel.Type || (current.KeyFingerprint != identity.KeyDigest && current.KeyFingerprint != identity.KeyDigest[:16]) {
				return false, ErrChannelAdmissionConflict
			}
			now, e := channelAdmissionTime(tx)
			if e != nil {
				return false, e
			}
			id, _ := admissionHash([]any{"image-legacy-adoption-v1", nonce, imageAdmissionResource(current)})
			initial = &ChannelAdmissionUse{ID: id, ResourceKey: imageAdmissionResource(current), RequestHash: imageAdmissionRequestHash(current), Epoch: c.Epoch, ChannelID: identity.ChannelID, KeyDigest: identity.KeyDigest, IdentityHash: identity.IdentityHash, Lane: "image", State: "open", AdmittedAtMs: now.UnixMilli()}
			if e = tx.Create(initial).Error; e != nil {
				return false, e
			}
			task.AdmissionGeneration = id
			task.KeyFingerprint = identity.KeyDigest
			input.ResourceKey = imageAttemptResource(task, index)
			input.RequestHash = imageAttemptHash(task, index, identity)
		}
		if initial.ChannelID != identity.ChannelID || initial.KeyDigest != identity.KeyDigest || initial.IdentityHash != identity.IdentityHash {
			return false, ErrChannelAdmissionConflict
		}
		if _, e = storedAdmissionIdentity(tx, identity); e != nil {
			return false, e
		}
		now, e := channelAdmissionTime(tx)
		if e != nil {
			return false, e
		}
		frozenID, _ = admissionHash([]any{input.Lane, input.GroupID, input.TokenID, input.RequestID})
		row := ChannelAdmissionUse{ID: frozenID, ResourceKey: input.ResourceKey, RequestHash: input.RequestHash, Epoch: c.Epoch, ChannelID: identity.ChannelID, KeyDigest: identity.KeyDigest, IdentityHash: identity.IdentityHash, Lane: "image", State: "open", AdmittedAtMs: now.UnixMilli()}
		if e = tx.Create(&row).Error; e != nil {
			return true, e
		}
		return true, checkImageAdmissionSubmitTime(tx, current)
	})
	if err != nil {
		return "", err
	}
	if frozenID != "" {
		return frozenID, nil
	}
	if use == nil {
		return "", nil
	}
	if !created {
		return "", ErrChannelAdmissionConflict
	}
	return use.ID, nil
}

func saveImageAttemptWithAdmission(task *ImageTask, expected, channelID int, baseURL string) error {
	store, err := channelAdmissionRuntimeStore(LogDB)
	if err != nil {
		return err
	}
	if store == nil {
		return saveImageTaskAttempt(LogDB, task, expected, channelID, baseURL)
	}
	return store.transaction(context.Background(), func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		inactive, e := channelAdmissionNeverActivated(tx, c)
		if e != nil {
			return e
		}
		if inactive {
			return saveImageTaskAttempt(tx, task, expected, channelID, baseURL)
		}
		current, e := lockAdmissionImageTask(tx, task)
		if e != nil {
			return e
		}
		newAttempt := len(task.Attempts) == expected+1
		if newAttempt {
			if e = checkImageAdmissionSubmitTime(tx, current); e != nil {
				return e
			}
		}
		if task.AdmissionAttempt == "" || len(task.Attempts) == 0 {
			return ErrChannelAdmissionConflict
		}
		var use ChannelAdmissionUse
		if e = tx.First(&use, "id = ?", task.AdmissionAttempt).Error; e != nil {
			return e
		}
		index := len(task.Attempts) - 1
		identity := ChannelIdentity{ChannelID: use.ChannelID, KeyDigest: use.KeyDigest, IdentityHash: use.IdentityHash}
		if use.Lane != "image" || use.State != "open" || use.ResourceKey != imageAttemptResource(task, index) || use.RequestHash != imageAttemptHash(task, index, identity) || use.ChannelID != channelID || use.KeyDigest != task.KeyFingerprint {
			return ErrChannelAdmissionConflict
		}
		channel, e := storedAdmissionIdentity(tx, identity)
		if e != nil {
			return e
		}
		if channel.BaseURL != baseURL {
			return ErrChannelAdmissionConflict
		}
		if e = saveImageTaskAttempt(tx, task, expected, channelID, baseURL); e != nil {
			return e
		}
		if newAttempt {
			return checkImageAdmissionSubmitTime(tx, current)
		}
		return nil
	}, true)
}

// Reuse the existing image submission deadline, evaluated after the row lock
// and again after a new attempt write. This does not change generation policy.
func checkImageAdmissionSubmitTime(tx *gorm.DB, task *ImageTask) error {
	now, err := channelAdmissionTime(tx)
	if err != nil {
		return err
	}
	if task.CreatedAt.IsZero() || task.CreatedAt.UnixMilli() > now.UnixMilli() || !now.Before(task.CreatedAt.Add(AsyncGenerationDeadline)) {
		return ErrChannelAdmissionConflict
	}
	return nil
}

// CaptureImageTaskGeneration runs before a recovery wallet call. A stale
// snapshot cannot acquire the replacement reservation's generation.
func CaptureImageTaskGeneration(task *ImageTask) error {
	store, err := channelAdmissionRuntimeStore(LogDB)
	if err != nil {
		return err
	}
	if store == nil {
		return nil
	}
	generation := task.AdmissionGeneration
	err = store.transaction(context.Background(), func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		inactive, e := channelAdmissionNeverActivated(tx, c)
		if e != nil || inactive {
			return e
		}
		use, e := imageReservationUse(tx, task)
		if e != nil {
			return e
		}
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
		return sameImageWalletSnapshot(current, task)
	}, true)
	if err == nil {
		task.AdmissionGeneration = generation
	}
	return err
}
func sameImageWalletSnapshot(current, observed *ImageTask) error {
	old, err := json.Marshal(observed.Attempts)
	if err != nil {
		return err
	}
	now, err := json.Marshal(current.Attempts)
	if err != nil {
		return err
	}
	if current.Status != observed.Status || string(old) != string(now) {
		return ErrChannelAdmissionConflict
	}
	return nil
}
func withImageTaskGeneration(task *ImageTask, write func(*gorm.DB) error) error {
	store, err := channelAdmissionRuntimeStore(LogDB)
	if err != nil {
		return err
	}
	if store == nil {
		return write(LogDB)
	}
	return store.transaction(context.Background(), func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		inactive, e := channelAdmissionNeverActivated(tx, c)
		if e != nil {
			return e
		}
		if inactive {
			return write(tx)
		}
		current, e := lockAdmissionImageTask(tx, task)
		if e != nil {
			return e
		}
		if e = sameImageWalletSnapshot(current, task); e != nil {
			return e
		}
		return write(tx)
	}, true)
}
