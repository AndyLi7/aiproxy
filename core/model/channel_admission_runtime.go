package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Runtime configuration is server-owned. No request, option switch or client
// header may replace it. Until a real trusted snapshot adapter is configured,
// an activated database refuses new work; a never-activated database keeps its
// legacy behavior. Tests install only explicitly synthetic local snapshots.
type channelAdmissionRuntime struct {
	executor ChannelExecutorIdentity
	load     func(context.Context) (VerifiedDeploymentProof, error)
}

var admissionRuntime atomic.Pointer[channelAdmissionRuntime]

// ConfigureChannelAdmissionRuntime installs the trusted, bounded local proof
// source at server bootstrap. It cannot be set by an HTTP API. The returned
// restore closure is for isolated tests; runtime code must never use it to
// disable an established fence.
func ConfigureChannelAdmissionRuntime(executor ChannelExecutorIdentity, load func(context.Context) (VerifiedDeploymentProof, error)) func() {
	previous := admissionRuntime.Swap(&channelAdmissionRuntime{executor: executor, load: load})
	return func() { admissionRuntime.Store(previous) }
}

// Query metadata explicitly: HasTable's bool-only API cannot distinguish
// absent legacy storage from an unavailable database. Any partial C storage
// requires the complete guarded path and therefore fails closed.
func channelAdmissionTablesExist(db *gorm.DB) (bool, error) {
	if db == nil {
		return false, nil
	}
	names := []string{"channel_admission_controls", "channel_admission_proofs", "channel_admission_families", "channel_admission_barriers", "channel_admission_uses", "channel_admission_executions", "channel_admission_execution_protocols", "channel_admission_task_executions", "channel_admission_dispatch_handoffs", "channel_admission_recovery_handoffs"}
	var count int64
	var result *gorm.DB
	switch db.Dialector.Name() {
	case "sqlite":
		result = db.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ?", names).Scan(&count)
	case "postgres":
		result = db.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name IN ?", names).Scan(&count)
	case "mysql":
		result = db.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ?", names).Scan(&count)
	default:
		return false, ErrChannelAdmissionUnavailable
	}
	return count > 0, result.Error
}

func channelAdmissionRuntimeStore(taskDB *gorm.DB) (*ChannelAdmissionStore, error) {
	primary, err := channelAdmissionTablesExist(DB)
	if err != nil {
		return nil, err
	}
	tasks := primary
	if taskDB != DB {
		tasks, err = channelAdmissionTablesExist(taskDB)
		if err != nil {
			return nil, err
		}
	}
	runtime := admissionRuntime.Load()
	if !primary && !tasks && runtime == nil {
		return nil, nil
	}
	if DB == nil || taskDB != DB {
		return nil, ErrChannelAdmissionUnavailable
	}
	store := &ChannelAdmissionStore{DB: DB, LogDB: taskDB}
	if runtime != nil {
		store.Executor = runtime.executor
		store.LoadDeployment = runtime.load
	}
	return store, nil
}

func channelAdmissionNeverActivated(tx *gorm.DB, c *ChannelAdmissionControl) (bool, error) {
	if c.Epoch != "" {
		return false, nil
	}
	if c.EpochSequence != 0 || c.Revision != 0 || c.ActiveProofID != "" || c.StorageGeneration != "" {
		return false, ErrChannelAdmissionUnavailable
	}
	for _, table := range []any{&ChannelAdmissionFamily{}, &ChannelAdmissionBarrier{}, &ChannelAdmissionUse{}} {
		var count int64
		if err := tx.Model(table).Count(&count).Error; err != nil {
			return false, err
		}
		if count != 0 {
			return false, ErrChannelAdmissionUnavailable
		}
	}
	return true, nil
}

// reserveNativeTaskWithAdmission is called by the actual model reservation
// entry point used by public native requests and private trials. Existing
// frozen reservations follow their own identity/owner/idempotency checks; a
// new task and its use record commit together before wallet/provider calls.
func reserveNativeTaskWithAdmission(db *gorm.DB, task NativeTask) (saved *NativeTask, created bool, err error) {
	store, err := channelAdmissionRuntimeStore(db)
	if err != nil {
		return nil, false, err
	}
	if store == nil {
		return reserveNativeTask(db, task)
	}
	// Existing frozen tasks need no deployment snapshot, even if that source
	// is unavailable. Recheck under the control lock; disappearance cannot turn
	// this optimization into authorization for a new task.
	var existingCount int64
	if err := db.Model(&NativeTask{}).Where("id = ?", task.ID).Count(&existingCount).Error; err != nil {
		return nil, false, err
	}
	if existingCount > 0 {
		store.LoadDeployment = nil
	}
	generation, err := newAdmissionGeneration()
	if err != nil {
		return nil, false, err
	}
	resource := nativeAdmissionResource(task.ID, task.GroupID, task.TokenID)
	input := ChannelAdmissionRequest{ResourceKey: resource, RequestID: generation, GroupID: task.GroupID, TokenID: task.TokenID, Lane: "native", RequestHash: task.Fingerprint,
		Identity: ChannelIdentity{ChannelID: task.ChannelID, KeyDigest: task.KeyFingerprint, IdentityHash: task.AdmissionIdentityHash}}
	reserve := func(tx *gorm.DB) error {
		var reserveErr error
		saved, created, reserveErr = reserveNativeTask(tx, task)
		return reserveErr
	}
	use, _, err := store.admit(db.Statement.Context, input, reserve, func(tx *gorm.DB, c *ChannelAdmissionControl) (bool, error) {
		inactive, e := channelAdmissionNeverActivated(tx, c)
		if e != nil {
			return false, e
		}
		if inactive {
			return true, reserve(tx)
		}
		// A previous durable reservation is the linearization point for old work,
		// including legacy tasks without an admission row. Never rebind it.
		var previous NativeTask
		e = tx.Select("id").Where("id = ?", task.ID).First(&previous).Error
		if e == nil {
			if e = reserve(tx); e != nil {
				return true, e
			}
			saved.AdmissionGeneration, e = nativeReservationGeneration(tx, saved)
			return true, e
		}
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return false, e
		}
		return false, allowReleasedNativeGeneration(tx, resource)
	})
	if err != nil {
		return nil, false, err
	}
	// A duplicate generation never authorizes another provider call.
	if use != nil && saved == nil {
		return nil, false, ErrNativeTaskConflict
	}
	if use != nil {
		saved.AdmissionGeneration = use.ID
	}
	return saved, created, nil
}

func newAdmissionGeneration() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
func nativeAdmissionResource(id, group string, token int) string {
	key, _ := admissionHash([]any{"native", group, token, id})
	return key
}
func nativeReservationReleaseHash(use ChannelAdmissionUse) string {
	hash, _ := admissionHash([]any{"native-unadmitted-release-v1", use.ID, use.ResourceKey, use.RequestHash, use.IdentityHash})
	return hash
}

// Missing task rows are not evidence of non-submission. Only our atomic,
// positively guarded release permits a later reservation generation.
func allowReleasedNativeGeneration(tx *gorm.DB, resource string) error {
	var uses []ChannelAdmissionUse
	if err := tx.Where("resource_key = ?", resource).Find(&uses).Error; err != nil {
		return err
	}
	for _, use := range uses {
		if use.Lane != "native" || use.State != "closed" || use.ClosedAtMs <= 0 || use.ClosureHash != nativeReservationReleaseHash(use) {
			return ErrChannelAdmissionRetained
		}
	}
	return nil
}
func releaseNativeReservationWithAdmission(db *gorm.DB, id, group string, token int, notAfter time.Time, evidence ...NativeReservationRelease) (released bool, err error) {
	store, err := channelAdmissionRuntimeStore(db)
	if err != nil {
		return false, err
	}
	if store == nil {
		return releaseNativeReservation(db, id, group, token, notAfter)
	}
	err = store.transaction(db.Statement.Context, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		inactive, err := channelAdmissionNeverActivated(tx, c)
		if err != nil {
			return err
		}
		if inactive {
			released, err = releaseNativeReservation(tx, id, group, token, notAfter)
			return err
		}
		task, err := lockAdmissionNativeTask(tx, id, group, token)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		// Even an expired owner may still be executing. Do not infer quiescence
		// from elapsed time when releasing the only durable task identity.
		if len(evidence) != 1 || !task.CreatedAt.Equal(evidence[0].CreatedAt) {
			return ErrChannelAdmissionConflict
		}
		generation, err := nativeReservationGeneration(tx, task)
		if err != nil {
			return err
		}
		if generation != evidence[0].GenerationID {
			return ErrChannelAdmissionConflict
		}
		now, err := channelAdmissionTime(tx)
		if err != nil {
			return err
		}
		if task.DispatchOwner != "" {
			return nil
		}
		if evidence[0].RecoveryOwner != "" {
			if task.RecoveryOwner != evidence[0].RecoveryOwner || task.RecoveryUntil <= now.Unix() {
				return ErrChannelAdmissionConflict
			}
		} else if task.RecoveryOwner != "" {
			return nil
		}
		releaseQuery := tx
		releaseCutoff := notAfter
		if evidence[0].RecoveryOwner != "" {
			if evidence[0].ReservationUpdatedAt.IsZero() || evidence[0].ReservationUpdatedAt.After(notAfter) || !task.UpdatedAt.Equal(evidence[0].ObservedUpdatedAt) {
				return nil
			}
			releaseCutoff = evidence[0].ObservedUpdatedAt
			releaseQuery = tx.Where("recovery_owner = ?", evidence[0].RecoveryOwner)
			switch tx.Dialector.Name() {
			case "postgres":
				releaseQuery = releaseQuery.Where("recovery_until > EXTRACT(EPOCH FROM clock_timestamp())")
			case "sqlite":
				releaseQuery = releaseQuery.Where("recovery_until > CAST(strftime('%s','now') AS INTEGER)")
			default:
				return ErrChannelAdmissionUnavailable
			}
		} else {
			releaseQuery = tx.Where("recovery_owner = ?", "")
		}
		released, err = releaseNativeReservation(releaseQuery, id, group, token, releaseCutoff)
		if err != nil || !released {
			return err
		}
		var uses []ChannelAdmissionUse
		if err := tx.Where("resource_key = ? AND state <> ?", nativeAdmissionResource(id, group, token), "closed").Find(&uses).Error; err != nil {
			return err
		}
		for _, use := range uses {
			if use.Lane != "native" || use.ChannelID != task.ChannelID || use.KeyDigest != task.KeyFingerprint || use.RequestHash != task.Fingerprint || use.State != "open" {
				return ErrChannelAdmissionConflict
			}
			result := tx.Model(&ChannelAdmissionUse{}).Where("id = ? AND state = ?", use.ID, "open").Updates(map[string]any{"state": "closed", "closed_at_ms": now.UnixMilli(), "closure_hash": nativeReservationReleaseHash(use)})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrChannelAdmissionConflict
			}
		}
		return nil
	}, true)
	if err != nil {
		return false, err
	}
	return released, nil
}

// NativeReservationRelease is captured before the wallet call. It is supplied
// only after a definite wallet refusal/not-found; a timeout is not evidence.
type NativeReservationRelease struct {
	GenerationID         string
	CreatedAt            time.Time
	RecoveryOwner        string
	ReservationUpdatedAt time.Time
	ObservedUpdatedAt    time.Time
}

func nativeReservationGeneration(tx *gorm.DB, task *NativeTask) (string, error) {
	var uses []ChannelAdmissionUse
	if err := tx.Where("resource_key = ? AND state <> ?", nativeAdmissionResource(task.ID, task.GroupID, task.TokenID), "closed").Find(&uses).Error; err != nil {
		return "", err
	}
	if len(uses) == 0 {
		return "", nil
	} // A pre-C task remains a legacy reservation.
	if len(uses) != 1 {
		return "", ErrChannelAdmissionConflict
	}
	use := uses[0]
	if use.Lane != "native" || use.State != "open" || use.ChannelID != task.ChannelID || use.KeyDigest != task.KeyFingerprint || use.RequestHash != task.Fingerprint {
		return "", ErrChannelAdmissionConflict
	}
	return use.ID, nil
}

// CaptureNativeReservationRelease reads the exact generation before recovery
// asks the wallet. The release rechecks generation, creation and live owner.
func CaptureNativeReservationRelease(db *gorm.DB, task *NativeTask, owner string) (evidence NativeReservationRelease, err error) {
	evidence = NativeReservationRelease{CreatedAt: task.CreatedAt, RecoveryOwner: owner, ReservationUpdatedAt: task.UpdatedAt}
	store, err := channelAdmissionRuntimeStore(db)
	if err != nil || store == nil {
		return evidence, err
	}
	err = store.transaction(db.Statement.Context, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		inactive, err := channelAdmissionNeverActivated(tx, c)
		if err != nil || inactive {
			return err
		}
		current, err := lockAdmissionNativeTask(tx, task.ID, task.GroupID, task.TokenID)
		if err != nil {
			return err
		}
		if !current.CreatedAt.Equal(task.CreatedAt) || current.RecoveryOwner != owner {
			return ErrChannelAdmissionConflict
		}
		if owner != "" {
			now, err := channelAdmissionTime(tx)
			if err != nil {
				return err
			}
			if current.RecoveryUntil <= now.Unix() {
				return ErrChannelAdmissionConflict
			}
		}
		evidence.ObservedUpdatedAt = current.UpdatedAt
		evidence.GenerationID, err = nativeReservationGeneration(tx, current)
		return err
	}, true)
	return evidence, err
}

// Full snapshot: the older log helper intentionally projects only call-row
// columns and must not be used for creation/generation/lease decisions.
func lockAdmissionNativeTask(tx *gorm.DB, id, group string, token int) (*NativeTask, error) {
	var task NativeTask
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND group_id = ? AND token_id = ?", id, group, token).First(&task).Error
	return &task, err
}

// WithNativeReservationGeneration fences a late positive wallet receipt as
// well as a refusal. The bounded callback performs DB writes only. A retired
// generation may neither submit nor enqueue a newly recreated reservation.
func WithNativeReservationGeneration(db *gorm.DB, task *NativeTask, write func(*gorm.DB) error) error {
	return withNativeReservationGeneration(db, task, write, write)
}

func withNativeReservationGeneration(db *gorm.DB, task *NativeTask, write, legacy func(*gorm.DB) error) error {
	store, err := channelAdmissionRuntimeStore(db)
	if err != nil {
		return err
	}
	if store == nil {
		return legacy(db)
	}
	return store.transaction(db.Statement.Context, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		inactive, err := channelAdmissionNeverActivated(tx, c)
		if err != nil {
			return err
		}
		if inactive {
			return legacy(tx)
		}
		current, err := lockAdmissionNativeTask(tx, task.ID, task.GroupID, task.TokenID)
		if err != nil {
			return err
		}
		if current.Status != "reserved" || !current.CreatedAt.Equal(task.CreatedAt) {
			return ErrNativeTaskConflict
		}
		generation, err := nativeReservationGeneration(tx, current)
		if err != nil {
			return err
		}
		if generation != task.AdmissionGeneration {
			return ErrNativeTaskConflict
		}
		if err := checkNativeAdmissionRecoveryLease(tx, task); err != nil {
			return err
		}
		if err := write(tx); err != nil {
			return err
		}
		// A bounded DB callback can itself wait. Recheck fresh database time before
		// committing, and roll back every callback write if the lease expired.
		return checkNativeAdmissionRecoveryLease(tx, task)
	}, true)
}

func checkNativeAdmissionRecoveryLease(tx *gorm.DB, task *NativeTask) error {
	if task.AdmissionRecoveryOwner == "" {
		return nil
	}
	query := tx.Model(&NativeTask{}).Where("id = ? AND group_id = ? AND token_id = ? AND recovery_owner = ?", task.ID, task.GroupID, task.TokenID, task.AdmissionRecoveryOwner)
	switch tx.Dialector.Name() {
	case "postgres":
		query = query.Where("recovery_until > EXTRACT(EPOCH FROM clock_timestamp())")
	case "sqlite":
		query = query.Where("recovery_until > CAST(strftime('%s','now') AS INTEGER)")
	default:
		return ErrChannelAdmissionUnavailable
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return ErrNativeTaskConflict
	}
	return nil
}

// AbandonNativeReservationGeneration records the terminal decision before any
// refund. Receipt equality prevents a stale not-submitted snapshot from
// overriding newer accepted/unknown wallet evidence in the same generation.
func AbandonNativeReservationGeneration(db *gorm.DB, task *NativeTask, reservedBefore, quietAt time.Time) (abandoned bool, err error) {
	legacy := func(tx *gorm.DB) error {
		var e error
		abandoned, e = AbandonNativeReservation(tx, task.ID, task.GroupID, task.TokenID, reservedBefore)
		return e
	}
	guarded := func(tx *gorm.DB) error {
		if task.AdmissionRecoveryOwner == "" {
			return ErrNativeTaskConflict
		}
		now, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		if quietAt.IsZero() || now.Before(quietAt) {
			return nil
		}
		var writeErr error
		abandoned, writeErr = AbandonNativeReservation(tx.Where("billing_receipt_json = ?", task.BillingReceiptJSON), task.ID, task.GroupID, task.TokenID, reservedBefore)
		return writeErr
	}
	err = withNativeReservationGeneration(db, task, guarded, legacy)
	if err != nil {
		return false, err
	}
	return abandoned, nil
}
