package model

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// This journal covers the owned body of one non-reserved recovery claim.
// Supplier work, unjoined goroutines and later invocations remain unproven.
type ChannelAdmissionRecoveryHandoff struct {
	ID                string `gorm:"primaryKey;size:64"`
	Version           int    `gorm:"not null"`
	ClaimHash         string `gorm:"size:64;not null;uniqueIndex"`
	ExecutionHash     string `gorm:"size:64;not null"`
	AuthorizationHash string `gorm:"size:64;not null"`
	State             string `gorm:"size:16;not null"`
	OfferedAtMs       int64  `gorm:"not null"`
	AcceptedAtMs      int64  `gorm:"not null"`
	ExitedAtMs        int64  `gorm:"not null"`
}

// Only a prepared in-process capability may authorize the claim body and
// its observing outer loop. The database stores a digest; JSON cannot recover a handle.
type NativeRecoveryHandoffHandle struct{ execution *NativeTaskExecutionHandle }

func recoveryHandoffClaim(row *ChannelAdmissionTaskExecution) string {
	hash, _ := admissionHash(
		[]any{"recovery-handoff-claim-v1", row.UseID, row.TaskHash, row.RecoveryOwner},
	)
	return hash
}

func recoveryHandoffAuthorization(row ChannelAdmissionRecoveryHandoff, nonce string) string {
	row.AuthorizationHash = ""
	row.State = "offered"
	row.AcceptedAtMs = 0
	row.ExitedAtMs = 0
	hash, _ := admissionHash([]any{"recovery-handoff-v1", row, nonce})

	return hash
}

func recoveryHandoffStoragePresent(tx *gorm.DB) (bool, error) {
	var (
		tables int64
		err    error
	)
	switch tx.Name() {
	case "sqlite":
		err = tx.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name = ?", "channel_admission_recovery_handoffs").
			Scan(&tables).
			Error
	case "postgres":
		err = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name = ?", "channel_admission_recovery_handoffs").
			Scan(&tables).
			Error
	default:
		return false, ErrChannelAdmissionUnavailable
	}

	if err != nil {
		return false, err
	}

	base, err := nativeExecutionStoragePresent(tx)
	if err != nil {
		return false, err
	}

	if !base {
		if tables != 0 {
			return false, ErrChannelAdmissionUnavailable
		}

		relayBase, err := channelExecutionStoragePresent(tx)
		if err != nil {
			return false, err
		}

		if relayBase {
			var orphan int64
			if err = tx.Model(&ChannelAdmissionExecutionProtocol{}).
				Where("id = ?", 4).
				Count(&orphan).
				Error; err != nil {
				return false, err
			}

			if orphan != 0 {
				return false, ErrChannelAdmissionUnavailable
			}
		}

		return false, nil
	}

	var protocols []ChannelAdmissionExecutionProtocol
	if err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", 4).
		Find(&protocols).
		Error; err != nil {
		return false, err
	}

	if tables == 0 && len(protocols) == 0 {
		return false, nil
	}

	if tables != 1 || len(protocols) != 1 || protocols[0].Version != 1 {
		return false, ErrChannelAdmissionUnavailable
	}

	statement := &gorm.Statement{DB: tx}
	if err = statement.Parse(&ChannelAdmissionRecoveryHandoff{}); err != nil {
		return false, err
	}

	rows, err := tx.Model(&ChannelAdmissionRecoveryHandoff{}).
		Select(statement.Schema.DBNames).
		Limit(0).
		Rows()
	if err != nil {
		return false, err
	}

	rowErr := rows.Err()
	//nolint:sqlclosecheck // The LIMIT 0 schema probe must close before the next query on this transaction.
	closeErr := rows.Close()

	if rowErr != nil {
		return false, rowErr
	}

	return true, closeErr
}

// PrepareNativeRecoveryHandoff runs after the existing unique recovery claim,
// before its body starts. Execution + offered commit atomically; duplicate preparation
// for the same claim fails instead of recovering or duplicating its capability.
// Nil means absent extension or previously uncovered frozen work. It never
// retroactively covers work that started before this registration.
func PrepareNativeRecoveryHandoff(
	ctx context.Context,
	db *gorm.DB,
	task *NativeTask,
) (*NativeRecoveryHandoffHandle, error) {
	if task == nil || task.Status == "reserved" || task.RecoveryOwner == "" ||
		task.RecoveryUntil <= 0 {
		return nil, ErrNativeTaskConflict
	}

	store, err := channelAdmissionRuntimeStore(db)
	if err != nil || store == nil {
		return nil, err
	}

	present, err := recoveryHandoffStoragePresent(db)
	if err != nil || !present {
		return nil, err
	}

	execution, err := beginNativeTaskExecution(
		ctx,
		db,
		task,
		NativeExecutionRecovery,
		task.RecoveryOwner,
		func(tx *gorm.DB) error {
			present, err := recoveryHandoffStoragePresent(tx)
			if err != nil {
				return err
			}

			if !present {
				return ErrChannelAdmissionUnavailable
			}

			return nil
		},
		func(tx *gorm.DB, c *ChannelAdmissionControl, row *ChannelAdmissionTaskExecution, nonce string) error {
			present, err := recoveryHandoffStoragePresent(tx)
			if err != nil {
				return err
			}

			if !present {
				return ErrChannelAdmissionUnavailable
			}

			if task.Status == "reserved" || task.RecoveryOwner == "" || task.RecoveryUntil <= 0 ||
				row.RecoveryOwner != task.RecoveryOwner ||
				row.RecoveryUntil != task.RecoveryUntil {
				return ErrNativeTaskConflict
			}

			handoff := ChannelAdmissionRecoveryHandoff{
				ID:            row.ID,
				Version:       1,
				ClaimHash:     recoveryHandoffClaim(row),
				ExecutionHash: row.AuthorizationHash,
				State:         "offered",
				OfferedAtMs:   row.StartedAtMs,
			}
			handoff.AuthorizationHash = recoveryHandoffAuthorization(handoff, nonce)

			var existing int64
			if err = tx.Model(&ChannelAdmissionRecoveryHandoff{}).
				Where("claim_hash = ?", handoff.ClaimHash).
				Count(&existing).
				Error; err != nil {
				return err
			}

			if existing != 0 {
				return ErrNativeTaskConflict
			}

			return tx.Create(&handoff).Error
		},
		task.RecoveryUntil,
	)
	if err != nil || execution == nil {
		return nil, err
	}

	return &NativeRecoveryHandoffHandle{execution: execution}, nil
}

func (h *NativeRecoveryHandoffHandle) withHandoff(
	ctx context.Context,
	work func(*gorm.DB, *NativeTask, *ChannelAdmissionTaskExecution, *ChannelAdmissionRecoveryHandoff) error,
) error {
	if h == nil || h.execution == nil {
		return ErrNativeTaskConflict
	}

	return h.execution.withExecution(
		ctx,
		func(tx *gorm.DB, c *ChannelAdmissionControl, task *NativeTask, execution *ChannelAdmissionTaskExecution) error {
			present, err := recoveryHandoffStoragePresent(tx)
			if err != nil {
				return err
			}

			if !present {
				return ErrChannelAdmissionUnavailable
			}

			var row ChannelAdmissionRecoveryHandoff
			if err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				First(&row, "id = ?", execution.ID).
				Error; err != nil {
				return err
			}

			if execution.Kind != string(NativeExecutionRecovery) || execution.RecoveryOwner == "" ||
				row.Version != 1 ||
				row.ClaimHash != recoveryHandoffClaim(execution) ||
				row.ExecutionHash != execution.AuthorizationHash ||
				row.OfferedAtMs != execution.StartedAtMs ||
				row.AuthorizationHash != recoveryHandoffAuthorization(row, h.execution.nonce) {
				return ErrNativeTaskConflict
			}

			return work(tx, task, execution, &row)
		},
	)
}

// Accept is single-use. A duplicate acknowledgement never authorizes a second
// body to run, including after a lost response. Checks use DB time after locks.
func (h *NativeRecoveryHandoffHandle) Accept(ctx context.Context) (accepted bool, err error) {
	err = h.withHandoff(
		ctx,
		func(tx *gorm.DB, task *NativeTask, execution *ChannelAdmissionTaskExecution, row *ChannelAdmissionRecoveryHandoff) error {
			if row.State == "accepted" && row.AcceptedAtMs >= row.OfferedAtMs &&
				row.ExitedAtMs == 0 {
				return nil
			}

			if row.State != "offered" || row.AcceptedAtMs != 0 || row.ExitedAtMs != 0 ||
				execution.State != "active" ||
				execution.ReturnedAtMs != 0 ||
				task.Status == "reserved" {
				return ErrNativeTaskConflict
			}

			now, err := channelAdmissionTime(tx)
			if err != nil {
				return err
			}

			if now.UnixMilli() < row.OfferedAtMs {
				return ErrNativeTaskConflict
			}

			result := tx.Model(&ChannelAdmissionRecoveryHandoff{}).
				Where("id = ? AND state = ? AND accepted_at_ms = ?", row.ID, "offered", 0).
				Updates(map[string]any{"state": "accepted", "accepted_at_ms": now.UnixMilli()})
			if result.Error != nil {
				return result.Error
			}

			if result.RowsAffected != 1 {
				return ErrNativeTaskConflict
			}

			if err = nativeExecutionLease(tx, task, execution); err != nil {
				return err
			}

			accepted = true

			return nil
		},
	)
	if err != nil {
		return false, err
	}

	return accepted, nil
}

// ReleaseAndRecordExit is called only after normal completion of the owned
// per-claim recovery body, including its cleanup. Lease release, scheduling and
// exit evidence commit together. A changed status requests a scheduling retry.
// A lost reply is not permission to run again; after release the capability no
// longer validates. Unknown/expired owners never gain exit evidence.
func (h *NativeRecoveryHandoffHandle) ReleaseAndRecordExit(
	ctx context.Context,
	expected *NativeTask,
	next time.Time,
	failed bool,
) (released bool, err error) {
	state := "returned"
	if failed {
		state = "error_returned"
	}

	err = h.withHandoff(
		ctx,
		func(tx *gorm.DB, task *NativeTask, execution *ChannelAdmissionTaskExecution, row *ChannelAdmissionRecoveryHandoff) error {
			if row.State != "accepted" || row.AcceptedAtMs < row.OfferedAtMs ||
				row.ExitedAtMs != 0 ||
				execution.State != "active" ||
				execution.ReturnedAtMs != 0 {
				return ErrNativeTaskConflict
			}

			if expected == nil || task.Status == "reserved" {
				return ErrNativeTaskConflict
			}

			if recoveryScheduleHash(task) != recoveryScheduleHash(expected) {
				return nil
			}
			// Keep the locked, currently valid deadline before clearing the lease. A
			// protected extension is allowed; a replacement claim never is.
			deadline := task.RecoveryUntil

			now, e := channelAdmissionTime(tx)
			if e != nil {
				return e
			}

			if now.UnixMilli() < row.AcceptedAtMs || deadline <= now.Unix() {
				return ErrNativeTaskConflict
			}

			result := tx.Model(&NativeTask{}).
				Where("id = ? AND group_id = ? AND token_id = ? AND recovery_owner = ? AND recovery_until = ?", task.ID, task.GroupID, task.TokenID, execution.RecoveryOwner, deadline).
				Updates(map[string]any{"recovery_owner": "", "recovery_until": 0, "next_recovery_at": next.Unix()})
			if result.Error != nil {
				return result.Error
			}

			if result.RowsAffected != 1 {
				return ErrNativeTaskConflict
			}

			result = tx.Model(&ChannelAdmissionRecoveryHandoff{}).
				Where("id = ? AND state = ? AND exited_at_ms = ?", row.ID, "accepted", 0).
				Updates(map[string]any{"state": "exited", "exited_at_ms": now.UnixMilli()})
			if result.Error != nil {
				return result.Error
			}

			if result.RowsAffected != 1 {
				return ErrNativeTaskConflict
			}

			result = tx.Model(&ChannelAdmissionTaskExecution{}).
				Where("id = ? AND state = ? AND returned_at_ms = ?", execution.ID, "active", 0).
				Updates(map[string]any{"state": state, "returned_at_ms": now.UnixMilli()})
			if result.Error != nil {
				return result.Error
			}

			if result.RowsAffected != 1 {
				return ErrNativeTaskConflict
			}
			// Fresh database time AFTER all writes, using the pre-release deadline.
			// Never validate against the cleared lease or a new execution window.
			var count int64

			e = tx.Model(&NativeTask{}).
				Where("id = ? AND group_id = ? AND token_id = ? AND recovery_owner = ? AND recovery_until = ? AND next_recovery_at = ?", task.ID, task.GroupID, task.TokenID, "", 0, next.Unix()).
				Where("? > "+admissionClockSQL(tx), deadline*1000).
				Count(&count).
				Error
			if e != nil {
				return e
			}

			if count != 1 {
				return ErrNativeTaskConflict
			}

			released = true

			return nil
		},
	)
	if err != nil {
		return false, err
	}

	return released, nil
}

// Exactly the task fields used by recoveryNext, plus the dispatch claim token
// so a same-status queue round trip cannot retain an obsolete wake-up time.
func recoveryScheduleHash(task *NativeTask) string {
	stamp := func(v *time.Time) int64 {
		if v == nil {
			return 0
		}
		return v.UnixMicro()
	}
	hash, _ := admissionHash(
		[]any{
			task.ID,
			task.GroupID,
			task.TokenID,
			task.Status,
			task.CreatedAt.UnixMicro(),
			task.UpstreamID,
			task.QueueScope,
			stamp(task.EnqueuedAt),
			task.DispatchOwner,
			stamp(task.DispatchUntil),
			task.BillingReceiptJSON,
		},
	)

	return hash
}
