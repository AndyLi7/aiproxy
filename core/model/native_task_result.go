package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/labring/aiproxy/core/common/nativeresult"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrNativeTaskConflict = errors.New("native task state conflict")

// NativeTask is additive storage, separate from ImageTask. Do not auto-migrate
// production until the native task executor and reviewed migration are ready.
// Provider bytes remain private until declared artifacts have been persisted.
type NativeTask struct {
	RecoveryOwner       string    `gorm:"size:64" json:"-"`
	RecoveryUntil       int64     `gorm:"index" json:"-"`
	NextRecoveryAt      int64     `gorm:"index" json:"-"`
	BillingSettled      bool      `gorm:"index" json:"-"`
	DeliveryBase        string    `gorm:"size:512" json:"-"`
	ArtifactManifest    string    `gorm:"type:text" json:"-"`
	DeliveredOutput     string    `gorm:"type:text" json:"-"`
	BillingReceiptJSON  string    `gorm:"type:text" json:"-"`
	FrozenContract      string    `gorm:"type:text" json:"-"`
	NativeInput         string    `gorm:"type:text" json:"-"`
	ChannelID           int       `json:"-"`
	Endpoint            string    `gorm:"size:256" json:"-"`
	KeyFingerprint      string    `gorm:"size:64" json:"-"`
	CredentialScope     string    `gorm:"size:128" json:"-"`
	PrepaymentQuoteJSON string    `gorm:"type:text" json:"-"`
	BillingOperationID  string    `gorm:"size:200" json:"-"`
	ErrorCode           string    `gorm:"size:64" json:"-"`
	UpstreamID          string    `gorm:"size:256" json:"-"`
	ID                  string    `gorm:"primaryKey;size:128" json:"-"`
	GroupID             string    `gorm:"size:64;not null;index" json:"-"`
	TokenID             int       `gorm:"not null" json:"-"`
	Model               string    `gorm:"size:256;not null" json:"-"`
	Fingerprint         string    `gorm:"size:64;not null" json:"-"`
	OutputSchema        string    `gorm:"type:text;not null" json:"-"`
	OutputSchemaHash    string    `gorm:"size:64;not null" json:"-"`
	Status              string    `gorm:"size:32;not null" json:"-"`
	NativeOutput        string    `gorm:"type:text" json:"-"`
	CreatedAt           time.Time `json:"-"`
	UpdatedAt           time.Time `json:"-"`
}

// ReserveNativeTask claims idempotency before any caller may reserve funds or
// submit upstream. created=false must never trigger a second paid submission.
func ReserveNativeTask(db *gorm.DB, task NativeTask) (*NativeTask, bool, error) {
	if task.ID == "" || len(task.ID) > 128 || task.GroupID == "" || len(task.GroupID) > 64 || task.TokenID <= 0 || task.Model == "" || len(task.Model) > 256 || task.Fingerprint == "" || len(task.Fingerprint) > 64 {
		return nil, false, ErrNativeTaskConflict
	}
	if _, err := nativeresult.Compile([]byte(task.OutputSchema)); err != nil {
		return nil, false, err
	}
	digest := sha256.Sum256([]byte(task.OutputSchema))
	task.OutputSchemaHash = hex.EncodeToString(digest[:])
	task.UpstreamID = ""
	task.Status = "reserved"
	task.NativeOutput = ""
	task.DeliveredOutput = ""
	task.ArtifactManifest = ""
	task.ErrorCode = ""
	task.BillingReceiptJSON = ""
	task.RecoveryOwner = ""
	task.RecoveryUntil = 0
	task.NextRecoveryAt = 0
	task.BillingSettled = false
	var saved NativeTask
	created := false
	err := db.Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&task)
		if result.Error != nil {
			return result.Error
		}
		created = result.RowsAffected == 1
		if !created {
			// A retry marks its reservation as in use, so stale-reservation cleanup
			// cannot delete it while this request is talking to the wallet.
			if err := tx.Model(&NativeTask{}).Where("id = ? AND group_id = ? AND token_id = ? AND status = ?", task.ID, task.GroupID, task.TokenID, "reserved").Update("updated_at", time.Now()).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("id = ? AND group_id = ? AND token_id = ?", task.ID, task.GroupID, task.TokenID).First(&saved).Error; err != nil {
			return ErrNativeTaskConflict
		}
		if saved.KeyFingerprint != task.KeyFingerprint || saved.DeliveryBase != task.DeliveryBase || saved.Fingerprint != task.Fingerprint || saved.Model != task.Model || saved.OutputSchemaHash != task.OutputSchemaHash || saved.FrozenContract != task.FrozenContract || saved.NativeInput != task.NativeInput || saved.ChannelID != task.ChannelID || saved.Endpoint != task.Endpoint || saved.CredentialScope != task.CredentialScope || saved.PrepaymentQuoteJSON != task.PrepaymentQuoteJSON || saved.BillingOperationID != task.BillingOperationID {
			return ErrNativeTaskConflict
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return &saved, created, nil
}

// ReleaseNativeReservation deletes a reservation the wallet never admitted, so
// a refused or abandoned submission leaves no row for recovery to poll. Only a
// reservation untouched since notAfter is removed: a concurrent retry of the
// same request refreshes it first and keeps it.
func ReleaseNativeReservation(db *gorm.DB, id, group string, token int, notAfter time.Time) (bool, error) {
	result := db.Where("id = ? AND group_id = ? AND token_id = ? AND status = ? AND upstream_id = ? AND updated_at <= ?", id, group, token, "reserved", "", notAfter).Delete(&NativeTask{})
	return result.RowsAffected == 1, result.Error
}

func GetNativeTask(db *gorm.DB, id, group string, token int) (*NativeTask, error) {
	var task NativeTask
	err := db.Where("id = ? AND group_id = ? AND token_id = ?", id, group, token).First(&task).Error
	return &task, err
}

// GetNativeTaskInGroup finds a task by ID within one customer group, for
// administrative reads on behalf of the group's owner across all their keys.
func GetNativeTaskInGroup(db *gorm.DB, id, group string) (*NativeTask, error) {
	var task NativeTask
	err := db.Where("id = ? AND group_id = ?", id, group).First(&task).Error
	return &task, err
}

// SaveNativeTaskResult atomically persists the complete original provider JSON.
// This is result_received, NOT publicly completed: artifact delivery and billing
// recovery must run before the executor can expose a completed result.
func SaveNativeTaskResult(db *gorm.DB, id, group string, token int, raw []byte) error {
	task, err := GetNativeTask(db, id, group, token)
	if err != nil {
		return err
	}
	validator, err := nativeresult.Compile([]byte(task.OutputSchema))
	if err != nil {
		return err
	}
	validated, err := validator.Validate(raw)
	if err != nil {
		return err
	}
	if task.Status == "result_received" {
		if bytes.Equal([]byte(task.NativeOutput), validated) {
			return nil
		}
		return ErrNativeTaskConflict
	}
	if task.Status != "running" && task.Status != "queued" {
		return ErrNativeTaskConflict
	}
	result := db.Model(&NativeTask{}).Where("id = ? AND group_id = ? AND token_id = ? AND status = ? AND output_schema_hash = ?", id, group, token, task.Status, task.OutputSchemaHash).Updates(map[string]any{"status": "result_received", "native_output": string(validated), "updated_at": time.Now()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrNativeTaskConflict
	}
	return nil
}

// AcceptNativeTask records the provider identity once; it cannot be retargeted
// after acceptance, including after process restart or completed delivery.
func AcceptNativeTask(db *gorm.DB, id, group string, token int, upstreamID string) error {
	if upstreamID == "" || len(upstreamID) > 256 {
		return ErrNativeTaskConflict
	}
	task, err := GetNativeTask(db, id, group, token)
	if err != nil {
		return err
	}
	if task.UpstreamID == upstreamID && task.Status != "reserved" {
		return nil
	}
	if (task.Status != "reserved" && task.Status != "submitting") || task.UpstreamID != "" {
		return ErrNativeTaskConflict
	}
	// created_at becomes the acceptance time, in the same compare-and-set that
	// records the provider identity: AsyncGenerationDeadline counts from here,
	// not from a reservation an earlier ambiguous attempt may have left behind.
	now := time.Now()
	result := db.Model(&NativeTask{}).Where("id = ? AND group_id = ? AND token_id = ? AND status = ? AND upstream_id = ?", id, group, token, task.Status, "").Updates(map[string]any{"status": "queued", "upstream_id": upstreamID, "created_at": now, "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrNativeTaskConflict
	}
	return nil
}

// TransitionNativeSubmission is a compare-and-set transition owned by the executor.
func TransitionNativeSubmission(db *gorm.DB, id, group string, token int, from, to, code string) error {
	allowed := (from == "reserved" && to == "submitting") || (from == "submitting" && (to == "submission_unknown" || to == "failed"))
	if !allowed {
		return ErrNativeTaskConflict
	}
	result := db.Model(&NativeTask{}).Where("id = ? AND group_id = ? AND token_id = ? AND status = ? AND upstream_id = ?", id, group, token, from, "").Updates(map[string]any{"status": to, "error_code": code, "updated_at": time.Now()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrNativeTaskConflict
	}
	return nil
}

func SaveNativeTaskBillingReceipt(db *gorm.DB, id, group string, token int, receipt string) error {
	result := db.Model(&NativeTask{}).Where("id = ? AND group_id = ? AND token_id = ?", id, group, token).Updates(map[string]any{"billing_receipt_json": receipt, "updated_at": time.Now()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrNativeTaskConflict
	}
	return nil
}

// UpdateNativePoll only advances accepted tasks; a stale queued response cannot
// regress running work, and terminal results cannot be overwritten by polling.
func UpdateNativePoll(db *gorm.DB, id, group string, token int, status, code string) error {
	if status != "queued" && status != "running" && status != "failed" {
		return ErrNativeTaskConflict
	}
	if status == "failed" && code != "upstream_task_failed" && code != "upstream_result_rejected" {
		return ErrNativeTaskConflict
	}
	task, err := GetNativeTask(db, id, group, token)
	if err != nil {
		return err
	}
	if task.Status == status || (task.Status == "running" && status == "queued") {
		return nil
	}
	if task.Status != "queued" && task.Status != "running" {
		return ErrNativeTaskConflict
	}
	r := db.Model(&NativeTask{}).Where("id = ? AND group_id = ? AND token_id = ? AND status = ?", id, group, token, task.Status).Updates(map[string]any{"status": status, "error_code": code, "updated_at": time.Now()})
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return ErrNativeTaskConflict
	}
	return nil
}

// FailAcceptedNativeTask ends an accepted task that can never deliver: the
// provider failed it, its result was permanently rejected, or a received result
// stayed undeliverable past the long recovery deadline. Waiting for the provider
// is ended by ExpireNativeGeneration instead. Billing then refunds the customer
// (platform_failure).
func FailAcceptedNativeTask(db *gorm.DB, id, group string, token int, code string) error {
	if code != "upstream_task_failed" && code != "upstream_result_rejected" {
		return ErrNativeTaskConflict
	}
	r := db.Model(&NativeTask{}).Where("id = ? AND group_id = ? AND token_id = ? AND status IN ?", id, group, token, []string{"queued", "running", "result_received"}).Updates(map[string]any{"status": "failed", "error_code": code, "updated_at": time.Now()})
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected == 1 {
		return nil
	}
	task, err := GetNativeTask(db, id, group, token)
	if err == nil && task.Status == "failed" {
		return nil
	}
	return ErrNativeTaskConflict
}

// ExpireNativeGeneration fails an accepted task the provider has not finished
// within AsyncGenerationDeadline of acceptance (created_at). It only moves
// queued or running rows with a provider identity that are past the deadline,
// so it and SaveNativeTaskResult exclude each other: a result saved first is
// delivered, and a result arriving after expiry is discarded. expired reports
// whether this call made the change; only that caller cancels upstream.
// Billing then refunds the customer in full (platform_failure).
func ExpireNativeGeneration(db *gorm.DB, id, group string, token int, now time.Time) (bool, error) {
	r := db.Model(&NativeTask{}).Where("id = ? AND group_id = ? AND token_id = ? AND upstream_id <> ? AND status IN ? AND created_at <= ?", id, group, token, "", []string{"queued", "running"}, now.Add(-AsyncGenerationDeadline)).Updates(map[string]any{"status": "failed", "error_code": AsyncGenerationTimeoutCode, "updated_at": time.Now()})
	return r.RowsAffected == 1, r.Error
}
