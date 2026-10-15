package model

import (
	"context"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Native execution is an additive subjournal: a frozen use may have many
// wallet/poll/delivery invocations. No row is a new submission grant or lease.
type ChannelAdmissionTaskExecution struct {
	ID                  string `gorm:"primaryKey;size:64"`
	UseID               string `gorm:"size:64;index;not null"`
	Version             int    `gorm:"not null"`
	Kind                string `gorm:"size:32;not null"`
	UseHash             string `gorm:"size:64;not null"`
	TaskHash            string `gorm:"size:64;not null"`
	TaskCreatedAtMicros int64  `gorm:"not null"`
	Epoch               string `gorm:"size:128;not null"`
	StorageGeneration   string `gorm:"size:128;not null"`
	ServiceID           string `gorm:"size:128;not null"`
	InstanceID          string `gorm:"size:128;not null"`
	BuildSHA            string `gorm:"size:64;not null"`
	RecoveryOwner       string `gorm:"size:64;not null"`
	RecoveryUntil       int64  `gorm:"not null"`
	DispatchOwner       string `gorm:"size:64;not null"`
	DispatchUntilMicros int64  `gorm:"not null"`
	AuthorizationHash   string `gorm:"size:64;not null"`
	State               string `gorm:"size:32;not null"`
	ContinuationState   string `gorm:"size:32;not null"`
	StartedAtMs         int64  `gorm:"not null"`
	ReturnedAtMs        int64  `gorm:"not null"`
}
type NativeExecutionKind string

const (
	NativeExecutionSubmit   NativeExecutionKind = "native_submit"
	NativeExecutionDispatch NativeExecutionKind = "native_dispatch"
	NativeExecutionPoll     NativeExecutionKind = "native_poll"
	NativeExecutionBilling  NativeExecutionKind = "native_billing"
	NativeExecutionDelivery NativeExecutionKind = "native_delivery"
)

type NativeTaskExecutionHandle struct {
	store                    *ChannelAdmissionStore
	id, nonce, taskID, group string
	token                    int
}

func nativeExecutionTaskHash(t *NativeTask) string {
	hash, _ := admissionHash([]any{t.ID, t.GroupID, t.TokenID, t.Model, t.Fingerprint, t.OutputSchemaHash, t.FrozenContract, t.NativeInput, t.ChannelID, t.Endpoint, t.KeyFingerprint, t.CredentialScope, t.PrepaymentQuoteJSON, t.BillingOperationID, t.DeliveryBase, t.QueueScope})
	return hash
}
func nativeExecutionAuthorization(row ChannelAdmissionTaskExecution, nonce string) string {
	row.AuthorizationHash = ""
	row.State = "active"
	row.ReturnedAtMs = 0
	hash, _ := admissionHash([]any{"native-execution-v1", row, nonce})
	return hash
}
func nativeExecutionStoragePresent(tx *gorm.DB) (bool, error) {
	var tables int64
	var e error
	switch tx.Dialector.Name() {
	case "sqlite":
		e = tx.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name = ?", "channel_admission_task_executions").Scan(&tables).Error
	case "postgres":
		e = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name = ?", "channel_admission_task_executions").Scan(&tables).Error
	default:
		return false, ErrChannelAdmissionUnavailable
	}
	if e != nil {
		return false, e
	}
	base, e := channelExecutionStoragePresent(tx)
	if e != nil {
		return false, e
	}
	if !base {
		if tables != 0 {
			return false, ErrChannelAdmissionUnavailable
		}
		return false, nil
	}
	var protocols []ChannelAdmissionExecutionProtocol
	if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", 2).Find(&protocols).Error; e != nil {
		return false, e
	}
	if tables == 0 && len(protocols) == 0 {
		return false, nil
	}
	if tables != 1 || len(protocols) != 1 || protocols[0].Version != 1 {
		return false, ErrChannelAdmissionUnavailable
	}
	statement := &gorm.Statement{DB: tx}
	if e = statement.Parse(&ChannelAdmissionTaskExecution{}); e != nil {
		return false, e
	}
	rows, e := tx.Model(&ChannelAdmissionTaskExecution{}).Select(statement.Schema.DBNames).Limit(0).Rows()
	if e != nil {
		return false, e
	}
	rowErr := rows.Err()
	closeErr := rows.Close()
	if rowErr != nil {
		return false, rowErr
	}
	return true, closeErr
}
func nativeExecutionLease(tx *gorm.DB, t *NativeTask, row *ChannelAdmissionTaskExecution) error {
	now, e := channelAdmissionTime(tx)
	if e != nil {
		return e
	}
	if row.RecoveryOwner != "" && (t.RecoveryOwner != row.RecoveryOwner || t.RecoveryUntil < row.RecoveryUntil || t.RecoveryUntil <= now.Unix()) {
		return ErrNativeTaskConflict
	}
	if row.DispatchOwner != "" && (t.DispatchOwner != row.DispatchOwner || t.DispatchUntil == nil || t.DispatchUntil.UnixMicro() != row.DispatchUntilMicros || !t.DispatchUntil.After(now)) {
		return ErrNativeTaskConflict
	}
	query := tx.Model(&NativeTask{}).Where("id = ? AND group_id = ? AND token_id = ?", t.ID, t.GroupID, t.TokenID)
	if row.RecoveryOwner != "" {
		query = query.Where("recovery_owner = ? AND recovery_until >= ?", row.RecoveryOwner, row.RecoveryUntil).Where("recovery_until * 1000 > " + admissionClockSQL(tx))
	}
	if row.DispatchOwner != "" {
		query = nativeDispatchLeaseLive(query.Where("dispatch_owner = ? AND dispatch_until = ?", row.DispatchOwner, t.DispatchUntil))
	}
	var count int64
	if e = query.Count(&count).Error; e != nil {
		return e
	}
	if count != 1 {
		return ErrNativeTaskConflict
	}
	return nil
}

// BeginNativeTaskExecution captures the existing frozen reservation under the
// admission control and task locks. It does not claim a queue/recovery lease.
// recoveryOwner must come from the engine's private recovery context, not a
// request or the fact that some other worker happens to own the task. Its
// original RecoveryUntil is a lower bound, not a renewable identity. The unique
// claim token stays fixed during protected monotonic lease extension. A fresh
// claim always gets a new token, including same-worker/same-second reacquisition.
func BeginNativeTaskExecution(ctx context.Context, db *gorm.DB, task *NativeTask, kind NativeExecutionKind, recoveryOwner string, recoveryUntil ...int64) (*NativeTaskExecutionHandle, error) {
	if task == nil {
		return nil, ErrNativeTaskConflict
	}
	switch kind {
	case NativeExecutionSubmit, NativeExecutionDispatch, NativeExecutionPoll, NativeExecutionBilling, NativeExecutionDelivery:
	default:
		return nil, ErrNativeTaskConflict
	}
	store, e := channelAdmissionRuntimeStore(db)
	if e != nil || store == nil {
		return nil, e
	}
	// Avoid consulting any deployment source for the accepted v1-only path.
	// This is an optimization only: an observed extension must still exist
	// and validate under the control lock below.
	installed, e := nativeExecutionStoragePresent(db)
	if e != nil || !installed {
		return nil, e
	}
	expectedRecoveryUntil := int64(0)
	if recoveryOwner != "" {
		if len(recoveryUntil) != 1 || recoveryUntil[0] <= 0 {
			return nil, ErrNativeTaskConflict
		}
		expectedRecoveryUntil = recoveryUntil[0]
	} else if len(recoveryUntil) > 1 || (len(recoveryUntil) == 1 && recoveryUntil[0] != 0) {
		return nil, ErrNativeTaskConflict
	}
	proof, proofErr := store.deployment(ctx)
	nonce, e := newAdmissionGeneration()
	if e != nil {
		return nil, e
	}
	var handle *NativeTaskExecutionHandle
	e = store.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		present, e := nativeExecutionStoragePresent(tx)
		if e != nil {
			return e
		}
		if !present {
			return ErrChannelAdmissionUnavailable
		}
		inactive, e := channelAdmissionNeverActivated(tx, c)
		if e != nil || inactive {
			return e
		}
		current, e := lockAdmissionNativeTask(tx, task.ID, task.GroupID, task.TokenID)
		if e != nil {
			return e
		}
		sameTime := current.CreatedAt.Equal(task.CreatedAt)
		if tx.Dialector.Name() == "postgres" {
			sameTime = current.CreatedAt.UnixMicro() == task.CreatedAt.UnixMicro()
		}
		if !sameTime || current.Status != task.Status || nativeExecutionTaskHash(current) != nativeExecutionTaskHash(task) {
			return ErrNativeTaskConflict
		}
		generation, e := nativeReservationGeneration(tx, current)
		if e != nil {
			return e
		}
		if task.AdmissionGeneration != "" && task.AdmissionGeneration != generation {
			return ErrNativeTaskConflict
		}
		if generation == "" {
			return nil
		} // Old uncovered task never gains historical coverage.
		var use ChannelAdmissionUse
		if e = tx.First(&use, "id = ?", generation).Error; e != nil {
			return e
		}
		if proofErr != nil {
			return proofErr
		}
		if proof.StorageGeneration != c.StorageGeneration || proof.Epoch != c.Epoch || proof.Sequence != c.EpochSequence || proof.MembershipHash != c.MembershipHash || proof.ExecutorFenceHash != c.ExecutorFenceHash || proof.ProofID != c.ActiveProofID || proof.StorageWatermark < 0 || c.Revision < proof.StorageWatermark {
			return ErrChannelAdmissionUnavailable
		}
		membership, _ := admissionHash(proof.Executors)
		member := false
		for _, candidate := range proof.Executors {
			if candidate == store.Executor {
				member = true
			}
		}
		if membership != c.MembershipHash || !member {
			return ErrChannelAdmissionUnavailable
		}
		if _, e = storedAdmissionIdentity(tx, ChannelIdentity{ChannelID: use.ChannelID, KeyDigest: use.KeyDigest, IdentityHash: use.IdentityHash}); e != nil {
			return e
		}
		if store.Executor.ServiceID == "" || store.Executor.InstanceID == "" || !fullIdentityDigest(store.Executor.BuildSHA) || store.Executor.Epoch != c.Epoch {
			return ErrChannelAdmissionUnavailable
		}
		now, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		id, _ := admissionHash([]any{"native-execution-id-v1", generation, kind, nonce})
		row := ChannelAdmissionTaskExecution{ID: id, UseID: generation, Version: 1, Kind: string(kind), UseHash: executionUseHash(&use), TaskHash: nativeExecutionTaskHash(current), TaskCreatedAtMicros: current.CreatedAt.UnixMicro(), Epoch: c.Epoch, StorageGeneration: c.StorageGeneration, ServiceID: store.Executor.ServiceID, InstanceID: store.Executor.InstanceID, BuildSHA: store.Executor.BuildSHA, State: "active", ContinuationState: "unproven", StartedAtMs: now.UnixMilli()}
		if recoveryOwner != "" {
			row.RecoveryOwner = recoveryOwner
			if current.RecoveryUntil < expectedRecoveryUntil {
				return ErrNativeTaskConflict
			}
			row.RecoveryUntil = current.RecoveryUntil
		}
		if kind == NativeExecutionDispatch {
			if task.DispatchOwner == "" || task.Status != NativeStatusDispatching || current.Status != NativeStatusDispatching || current.DispatchUntil == nil {
				return ErrNativeTaskConflict
			}
			row.DispatchOwner = task.DispatchOwner
			row.DispatchUntilMicros = current.DispatchUntil.UnixMicro()
		}
		if e = nativeExecutionLease(tx, current, &row); e != nil {
			return e
		}
		row.AuthorizationHash = nativeExecutionAuthorization(row, nonce)
		if e = tx.Create(&row).Error; e != nil {
			return e
		}
		if e = nativeExecutionLease(tx, current, &row); e != nil {
			return e
		}
		handle = &NativeTaskExecutionHandle{store: store, id: id, nonce: nonce, taskID: task.ID, group: task.GroupID, token: task.TokenID}
		return nil
	}, true)
	if e != nil {
		return nil, e
	}
	return handle, nil
}

// RecordReturn is a call-scope fact only. It never settles money, authorizes a
// provider retry, releases the owner's lease, closes a use or proves no other
// worker exists. Lost leases, replacement generations and missing tasks stay
// active/unproven until a separate trustworthy recovery/fence can reconcile.
func (h *NativeTaskExecutionHandle) RecordReturn(ctx context.Context, failed bool) error {
	if h == nil || h.store == nil || h.id == "" || h.nonce == "" {
		return ErrNativeTaskConflict
	}
	state := "returned"
	if failed {
		state = "error_returned"
	}
	return h.store.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		present, e := nativeExecutionStoragePresent(tx)
		if e != nil {
			return e
		}
		if !present {
			return ErrChannelAdmissionUnavailable
		}
		task, e := lockAdmissionNativeTask(tx, h.taskID, h.group, h.token)
		if e != nil {
			return e
		}
		var row ChannelAdmissionTaskExecution
		if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", h.id).Error; e != nil {
			return e
		}
		if row.Version != 1 || row.ContinuationState != "unproven" || row.AuthorizationHash != nativeExecutionAuthorization(row, h.nonce) || row.StorageGeneration != c.StorageGeneration || nativeExecutionTaskHash(task) != row.TaskHash {
			return ErrNativeTaskConflict
		}
		generation, e := nativeReservationGeneration(tx, task)
		if e != nil {
			return e
		}
		if generation != row.UseID {
			return ErrNativeTaskConflict
		}
		var use ChannelAdmissionUse
		if e = tx.First(&use, "id = ?", row.UseID).Error; e != nil {
			return e
		}
		if executionUseHash(&use) != row.UseHash {
			return ErrNativeTaskConflict
		}
		if _, e = storedAdmissionIdentity(tx, ChannelIdentity{ChannelID: use.ChannelID, KeyDigest: use.KeyDigest, IdentityHash: use.IdentityHash}); e != nil {
			return e
		}
		if e = nativeExecutionLease(tx, task, &row); e != nil {
			return e
		}
		if row.State == state && row.ReturnedAtMs >= row.StartedAtMs {
			return nil
		}
		if row.State != "active" || row.ReturnedAtMs != 0 {
			return ErrNativeTaskConflict
		}
		now, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		if now.UnixMilli() < row.StartedAtMs {
			return ErrNativeTaskConflict
		}
		result := tx.Model(&ChannelAdmissionTaskExecution{}).Where("id = ? AND state = ? AND returned_at_ms = ? AND authorization_hash = ?", row.ID, "active", 0, row.AuthorizationHash).Updates(map[string]any{"state": state, "returned_at_ms": now.UnixMilli()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrNativeTaskConflict
		}
		return nativeExecutionLease(tx, task, &row)
	})
}
