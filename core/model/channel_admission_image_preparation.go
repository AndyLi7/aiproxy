package model

import (
	"encoding/json"

	"github.com/labring/aiproxy/core/common/failover"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const imagePreparationMetadataKey = "image_admission_preparation_v1"

// The random nonce stays inside the submitting process, never in JSON or log
// metadata. Its hash binds the exact durable pre-C reservation and full
// server-selected identity. A crash loses authority to send, not the task.
type imageAdmissionPreparation struct {
	nonce    string
	hash     string
	identity ChannelIdentity
	logID    int
}

func imagePreparationHash(
	task *ImageTask,
	identity ChannelIdentity,
	logID int,
	baseURL, nonce string,
) string {
	hash, _ := admissionHash(
		[]any{
			"image-pre-c-first-attempt-v1",
			nonce,
			task.ID,
			task.GroupID,
			task.TokenID,
			task.Model,
			task.Fingerprint,
			task.BillingOperationID,
			task.UsageID,
			task.CreatedAt.UTC().UnixMicro(),
			task.ChannelType,
			task.KeyFingerprint,
			task.UpstreamModel,
			logID,
			baseURL,
			identity,
		},
	)

	return hash
}

func imagePreparationRoute(
	tx *gorm.DB,
	task *ImageTask,
	identity ChannelIdentity,
) (*AsyncUsageInfo, error) {
	var info AsyncUsageInfo
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&info, "id = ? AND image_task_id = ?", task.UsageID, task.ID).
		Error; err != nil {
		return nil, err
	}

	if info.Status != AsyncUsageStatusNone || info.ChannelID != identity.ChannelID ||
		info.GroupID != task.GroupID ||
		info.TokenID != task.TokenID ||
		info.LogID == 0 ||
		(task.KeyFingerprint != identity.KeyDigest && task.KeyFingerprint != identity.KeyDigest[:16]) {
		return nil, ErrChannelAdmissionConflict
	}

	return &info, nil
}

// Record the first-call qualification before the external wallet claim, even
// when C storage is not installed/activated yet. Use the existing reservation
// audit row: no request-time migration, no client-readable nonce and no key.
//
//nolint:gocyclo // Legacy compatibility and identity checks must finish under the preparation locks before an attempt is persisted.
func prepareLegacyImageAdmission(
	database *gorm.DB,
	task *ImageTask,
	identity ChannelIdentity,
	index int,
) error {
	// Old synthetic/incomplete legacy rows continue their old path; they do not
	// gain a certificate that could authorize a protected first send later.
	if index != 0 || !validChannelIdentity(identity) || task.KeyFingerprint == "" ||
		task.UsageID <= 0 {
		return nil
	}

	if database.Name() != "sqlite" && database.Name() != "postgres" {
		return nil
	}

	nonce, err := newAdmissionGeneration()
	if err != nil {
		return err
	}

	var prepared *imageAdmissionPreparation

	err = database.Transaction(func(tx *gorm.DB) error {
		var current ImageTask
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&current, "id = ? AND group_id = ? AND token_id = ?", task.ID, task.GroupID, task.TokenID).
			Error; e != nil {
			return e
		}

		if !imageAdmissionCreatedAtEqual(tx, current.CreatedAt, task.CreatedAt) ||
			current.Fingerprint != task.Fingerprint ||
			current.Model != task.Model ||
			current.BillingOperationID != task.BillingOperationID ||
			current.UsageID != task.UsageID ||
			current.Status != "submitting" ||
			current.UpstreamID != "" ||
			len(current.Attempts) != 0 {
			return ErrChannelAdmissionConflict
		}

		info, e := imagePreparationRoute(tx, &current, identity)
		if e != nil {
			return e
		}

		if current.ChannelType != task.ChannelType ||
			current.KeyFingerprint != task.KeyFingerprint ||
			current.UpstreamModel != task.UpstreamModel {
			return ErrChannelAdmissionConflict
		}

		var entry Log
		if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&entry, "id = ? AND request_id = ? AND group_id = ? AND token_id = ?", info.LogID, task.ID, task.GroupID, task.TokenID).
			Error; e != nil {
			return e
		}

		if e = checkImageAdmissionSubmitTime(tx, &current); e != nil {
			return e
		}

		if entry.Metadata == nil {
			entry.Metadata = map[string]string{}
		}

		hash := imagePreparationHash(&current, identity, info.LogID, info.BaseURL, nonce)
		if existing := entry.Metadata[imagePreparationMetadataKey]; existing != "" {
			if task.admissionPreparation != nil && existing == task.admissionPreparation.hash &&
				existing == imagePreparationHash(
					&current,
					identity,
					info.LogID,
					info.BaseURL,
					task.admissionPreparation.nonce,
				) {
				prepared = task.admissionPreparation
				return nil
			}

			return ErrChannelAdmissionConflict
		}

		entry.Metadata[imagePreparationMetadataKey] = hash

		raw, e := json.Marshal(entry.Metadata)
		if e != nil {
			return e
		}

		if e = tx.Model(&Log{}).
			Where("id = ?", entry.ID).
			Update("metadata", string(raw)).
			Error; e != nil {
			return e
		}

		if e = checkImageAdmissionSubmitTime(tx, &current); e != nil {
			return e
		}

		prepared = &imageAdmissionPreparation{
			nonce:    nonce,
			hash:     hash,
			identity: identity,
			logID:    info.LogID,
		}

		return nil
	})
	if err == nil {
		task.admissionPreparation = prepared
	}

	return err
}

// The wallet has already returned claimed:true. Continue exactly that first
// attempt under the now-active control lock, without sending another claim or
// rechecking a family grant that did not apply to the frozen reservation.
func adoptPreparedImageAttempt(
	tx *gorm.DB,
	c *ChannelAdmissionControl,
	task *ImageTask,
	expected, channelID int,
	baseURL string,
) error {
	proof := task.admissionPreparation
	if proof == nil || task.AdmissionGeneration != "" || expected != 0 || len(task.Attempts) != 1 ||
		task.Attempts[0].ChannelID != channelID ||
		task.Attempts[0].Failure.Acceptance != failover.Unknown {
		return ErrChannelAdmissionConflict
	}

	current, err := lockAdmissionImageTask(tx, task)
	if err != nil {
		return err
	}

	if current.Status != "submitting" || current.UpstreamID != "" || len(current.Attempts) != 0 {
		return ErrChannelAdmissionConflict
	}

	identity := proof.identity
	if identity.ChannelID != channelID || !validChannelIdentity(identity) {
		return ErrChannelAdmissionConflict
	}

	info, err := imagePreparationRoute(tx, current, identity)
	if err != nil {
		return err
	}

	if info.LogID != proof.logID || info.BaseURL != baseURL {
		return ErrChannelAdmissionConflict
	}

	channel, err := storedAdmissionIdentity(tx, identity)
	if err != nil {
		return err
	}

	if channel.BaseURL != baseURL || current.ChannelType != channel.Type {
		return ErrChannelAdmissionConflict
	}

	var entry Log
	if err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&entry, "id = ? AND request_id = ? AND group_id = ? AND token_id = ?", proof.logID, task.ID, task.GroupID, task.TokenID).
		Error; err != nil {
		return err
	}

	if entry.Metadata[imagePreparationMetadataKey] != proof.hash ||
		proof.hash != imagePreparationHash(
			current,
			identity,
			info.LogID,
			info.BaseURL,
			proof.nonce,
		) {
		return ErrChannelAdmissionConflict
	}

	if err = checkImageAdmissionSubmitTime(tx, current); err != nil {
		return err
	}

	now, err := channelAdmissionTime(tx)
	if err != nil {
		return err
	}

	generation, _ := admissionHash([]any{"image-pre-c-reservation-v1", proof.hash})

	first := ChannelAdmissionUse{
		ID:           generation,
		ResourceKey:  imageAdmissionResource(current),
		RequestHash:  imageAdmissionRequestHash(current),
		Epoch:        c.Epoch,
		ChannelID:    identity.ChannelID,
		KeyDigest:    identity.KeyDigest,
		IdentityHash: identity.IdentityHash,
		Lane:         "image",
		State:        "open",
		AdmittedAtMs: now.UnixMilli(),
	}
	if err = tx.Create(&first).Error; err != nil {
		return err
	}

	task.AdmissionGeneration = generation
	task.KeyFingerprint = identity.KeyDigest
	attemptID, _ := admissionHash([]any{"image-pre-c-attempt-v1", proof.hash})
	attempt := first
	attempt.ID = attemptID
	attempt.ResourceKey = imageAttemptResource(task, 0)

	attempt.RequestHash = imageAttemptHash(task, 0, identity)
	if err = tx.Create(&attempt).Error; err != nil {
		return err
	}

	task.AdmissionAttempt = attemptID

	return checkImageAdmissionSubmitTime(tx, current)
}
