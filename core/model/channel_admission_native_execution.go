package model

import (
	"context"
	"encoding/json"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Native execution is an additive subjournal: a frozen use may have many
// wallet/poll/delivery invocations. No row is a new submission grant or lease.
const NativeExecutionProtocolVersion = 2

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
	ExecutorProofJSON   string `gorm:"type:text;not null"`
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
	hash, _ := admissionHash([]any{"native-execution-v2", row, nonce})
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
	if tables != 1 || len(protocols) != 1 || protocols[0].Version != NativeExecutionProtocolVersion {
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
	return beginNativeTaskExecution(ctx, db, task, kind, recoveryOwner, nil, nil, recoveryUntil...)
}

func beginNativeTaskExecution(ctx context.Context, db *gorm.DB, task *NativeTask, kind NativeExecutionKind, recoveryOwner string, onValidate func(*gorm.DB) error, onCreate func(*gorm.DB, *ChannelAdmissionControl, *ChannelAdmissionTaskExecution, string) error, recoveryUntil ...int64) (*NativeTaskExecutionHandle, error) {
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
	// An existing invocation is durable identity evidence for this frozen
	// use, not a fresh deployment/admission grant. Avoid loading the source
	// at all when an exact local executor already has recorded evidence.
	var priorCount int64
	e = db.WithContext(ctx).Model(&ChannelAdmissionTaskExecution{}).
		Where("use_id IN (?)", db.Model(&ChannelAdmissionUse{}).Select("id").Where("resource_key = ? AND state = ?", nativeAdmissionResource(task.ID, task.GroupID, task.TokenID), "open")).
		Where("service_id = ? AND instance_id = ? AND build_sha = ? AND epoch = ?", store.Executor.ServiceID, store.Executor.InstanceID, store.Executor.BuildSHA, store.Executor.Epoch).Count(&priorCount).Error
	if e != nil {
		return nil, e
	}
	var proof VerifiedDeploymentProof
	proofErr := ErrChannelAdmissionUnavailable
	if priorCount == 0 {
		proof, proofErr = store.deployment(ctx)
	}
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
		if onValidate != nil {
			if e = onValidate(tx); e != nil {
				return e
			}
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
		if _, e = storedAdmissionIdentity(tx, ChannelIdentity{ChannelID: use.ChannelID, KeyDigest: use.KeyDigest, IdentityHash: use.IdentityHash}); e != nil {
			return e
		}
		if store.Executor.ServiceID == "" || len(store.Executor.ServiceID) > 128 || store.Executor.InstanceID == "" || len(store.Executor.InstanceID) > 128 || !fullIdentityDigest(store.Executor.BuildSHA) || store.Executor.Epoch != c.Epoch {
			return ErrChannelAdmissionUnavailable
		}
		now, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		id, _ := admissionHash([]any{"native-execution-id-v1", generation, kind, nonce})
		row := ChannelAdmissionTaskExecution{ID: id, UseID: generation, Version: NativeExecutionProtocolVersion, Kind: string(kind), UseHash: executionUseHash(&use), TaskHash: nativeExecutionTaskHash(current), TaskCreatedAtMicros: current.CreatedAt.UnixMicro(), Epoch: c.Epoch, StorageGeneration: c.StorageGeneration, ServiceID: store.Executor.ServiceID, InstanceID: store.Executor.InstanceID, BuildSHA: store.Executor.BuildSHA, State: "active", ContinuationState: "unproven", StartedAtMs: now.UnixMilli()}
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
		binding, e := frozenNativeExecutionBinding(tx, c, &use, current, store.Executor, proof, proofErr)
		if e != nil {
			return e
		}
		if binding.source == "" {
			// No prior coverage and no snapshot: preserve the accepted frozen
			// business path, without inventing an executor grant or history.
			// The durable open use is retained and cannot prove drain/closure.
			return nativeExecutionLease(tx, current, &row)
		}
		row.ExecutorProofJSON = binding.proofJSON
		row.AuthorizationHash = nativeExecutionAuthorization(row, nonce)
		if e = tx.Create(&row).Error; e != nil {
			return e
		}
		if onCreate != nil {
			if e = onCreate(tx, c, &row, nonce); e != nil {
				return e
			}
		}
		if e = nativeExecutionLease(tx, current, &row); e != nil {
			return e
		}
		if binding.source == "snapshot" {
			if e = finalizeAdmissionProof(tx, c, proof, c.Revision); e != nil {
				return e
			}
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
	return h.withExecution(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl, task *NativeTask, row *ChannelAdmissionTaskExecution) error {
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
		return nativeExecutionLease(tx, task, row)
	})
}

func (h *NativeTaskExecutionHandle) withExecution(ctx context.Context, work func(*gorm.DB, *ChannelAdmissionControl, *NativeTask, *ChannelAdmissionTaskExecution) error) error {
	if h == nil || h.store == nil || h.id == "" || h.nonce == "" {
		return ErrNativeTaskConflict
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
		if row.Version != NativeExecutionProtocolVersion || row.ContinuationState != "unproven" || row.AuthorizationHash != nativeExecutionAuthorization(row, h.nonce) || row.StorageGeneration != c.StorageGeneration || nativeExecutionTaskHash(task) != row.TaskHash {
			return ErrNativeTaskConflict
		}
		if _, e = nativeExecutionHistoricalProof(tx, c, &row); e != nil {
			return e
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
		return work(tx, c, task, &row)
	})
}

// A binding records immutable historical membership facts. It never restores
// the invocation nonce, grants new admission, or certifies continuous coverage.
type nativeExecutionBinding struct {
	source, proofJSON string
}

// Historical proof validity is checked at creation with lockDeploymentProof
// and the final CAS. Its normal expiry does not invalidate past membership.
// Explicit revocation, loss or mutation of those trusted facts is different:
// retain the unknown invocation and reject rather than treating it as outage.
func nativeExecutionHistoricalProof(tx *gorm.DB, c *ChannelAdmissionControl, row *ChannelAdmissionTaskExecution) (VerifiedDeploymentProof, error) {
	var proof VerifiedDeploymentProof
	if row.ExecutorProofJSON == "" || json.Unmarshal([]byte(row.ExecutorProofJSON), &proof) != nil {
		return proof, ErrChannelAdmissionConflict
	}
	if proof.ProofID == "" || len(proof.ProofID) > 128 || proof.Sequence <= 0 || proof.Sequence > c.EpochSequence || proof.Epoch == "" || proof.Epoch != row.Epoch || proof.StorageGeneration != row.StorageGeneration || proof.StorageGeneration != c.StorageGeneration || proof.StorageWatermark < 0 || proof.StorageWatermark > c.Revision || len(proof.Executors) == 0 || len(proof.Executors) > 1024 || !fullIdentityDigest(proof.ExecutorFenceHash) {
		return proof, ErrChannelAdmissionConflict
	}
	if proof.Sequence == c.EpochSequence && (proof.Epoch != c.Epoch || proof.ProofID != c.ActiveProofID || proof.MembershipHash != c.MembershipHash || proof.ExecutorFenceHash != c.ExecutorFenceHash) {
		return proof, ErrChannelAdmissionConflict
	}
	membership, err := admissionHash(proof.Executors)
	if err != nil || membership != proof.MembershipHash {
		return proof, ErrChannelAdmissionConflict
	}
	member := false
	for _, candidate := range proof.Executors {
		if candidate.ServiceID == "" || len(candidate.ServiceID) > 128 || candidate.InstanceID == "" || len(candidate.InstanceID) > 128 || !fullIdentityDigest(candidate.BuildSHA) || candidate.Epoch != proof.Epoch {
			return proof, ErrChannelAdmissionConflict
		}
		if candidate.ServiceID == row.ServiceID && candidate.InstanceID == row.InstanceID && candidate.BuildSHA == row.BuildSHA {
			member = true
		}
	}
	if !member || proof.IssuedAtMs <= 0 || proof.ValidUntilMs <= proof.IssuedAtMs || proof.RevocationFreshUntilMs <= proof.IssuedAtMs {
		return proof, ErrChannelAdmissionConflict
	}
	hash, err := proof.Fingerprint()
	if err != nil {
		return proof, err
	}
	var stored ChannelAdmissionProof
	if err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&stored, "id = ?", proof.ProofID).Error; err != nil {
		return proof, err
	}
	if stored.FactHash != hash || stored.ConsumedSequence != proof.Sequence || stored.Revoked {
		return proof, ErrChannelAdmissionConflict
	}
	return proof, nil
}

func frozenNativeExecutionBinding(tx *gorm.DB, c *ChannelAdmissionControl, use *ChannelAdmissionUse, task *NativeTask, executor ChannelExecutorIdentity, proof VerifiedDeploymentProof, proofErr error) (nativeExecutionBinding, error) {
	var rows []ChannelAdmissionTaskExecution
	if err := tx.Where("use_id = ?", use.ID).Find(&rows).Error; err != nil {
		return nativeExecutionBinding{}, err
	}
	binding := nativeExecutionBinding{}
	for _, row := range rows {
		if row.Version != NativeExecutionProtocolVersion || row.UseHash != executionUseHash(use) || row.TaskHash != nativeExecutionTaskHash(task) || row.StorageGeneration != c.StorageGeneration || row.ContinuationState != "unproven" || row.StartedAtMs <= 0 || !fullIdentityDigest(row.AuthorizationHash) {
			return nativeExecutionBinding{}, ErrChannelAdmissionConflict
		}
		switch NativeExecutionKind(row.Kind) {
		case NativeExecutionSubmit, NativeExecutionDispatch, NativeExecutionPoll, NativeExecutionBilling, NativeExecutionDelivery:
		default:
			return nativeExecutionBinding{}, ErrChannelAdmissionConflict
		}
		if (row.State == "active" && row.ReturnedAtMs != 0) || ((row.State == "returned" || row.State == "error_returned") && row.ReturnedAtMs < row.StartedAtMs) || (row.State != "active" && row.State != "returned" && row.State != "error_returned") {
			return nativeExecutionBinding{}, ErrChannelAdmissionConflict
		}
		if _, err := nativeExecutionHistoricalProof(tx, c, &row); err != nil {
			return nativeExecutionBinding{}, err
		}
		// An older epoch's facts are valid history, but never the current
		// executor's proof-outage binding, even when process labels repeat.
		if row.Epoch == c.Epoch && row.ServiceID == executor.ServiceID && row.InstanceID == executor.InstanceID && row.BuildSHA == executor.BuildSHA {
			binding = nativeExecutionBinding{source: "durable", proofJSON: row.ExecutorProofJSON}
		}
	}
	if binding.source != "" {
		return binding, nil
	}
	var stored ChannelAdmissionProof
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&stored, "id = ?", c.ActiveProofID).Error; err != nil {
		return nativeExecutionBinding{}, err
	}
	if stored.Revoked || stored.ConsumedSequence != c.EpochSequence || !fullIdentityDigest(stored.FactHash) {
		return nativeExecutionBinding{}, ErrChannelAdmissionUnavailable
	}
	if proofErr != nil {
		hash, _ := proof.Fingerprint()
		emptyHash, _ := (VerifiedDeploymentProof{}).Fingerprint()
		if hash != emptyHash || errors.Is(proofErr, ErrChannelAdmissionConflict) || errors.Is(proofErr, ErrChannelAdmissionSealed) || len(rows) != 0 || use.Epoch != c.Epoch {
			return nativeExecutionBinding{}, ErrChannelAdmissionUnavailable
		}
		return nativeExecutionBinding{}, nil
	}
	if _, err := lockDeploymentProof(tx, c, proof, executor); err != nil {
		return nativeExecutionBinding{}, err
	}
	raw, err := json.Marshal(proof)
	if err != nil {
		return nativeExecutionBinding{}, err
	}
	return nativeExecutionBinding{source: "snapshot", proofJSON: string(raw)}, nil
}
