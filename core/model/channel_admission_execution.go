package model

import (
	"context"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ChannelAdmissionExecutionProtocol versions the additive execution journal.
// It is installed only by reviewed migration, never by request handlers.
type ChannelAdmissionExecutionProtocol struct {
	ID      int `gorm:"primaryKey;autoIncrement:false"`
	Version int `gorm:"not null"`
}

// ChannelAdmissionExecution records only the relay call stack and response-body
// cleanup. ContinuationState stays unproven: adaptor goroutines, upstream jobs,
// polling and billing are NOT certified finished by a returned call. Active
// rows have no TTL; missing rows never prove a drained use.
type ChannelAdmissionExecution struct {
	ID                string `gorm:"primaryKey;size:64"`
	UseID             string `gorm:"uniqueIndex;size:64;not null"`
	Version           int    `gorm:"not null"`
	Kind              string `gorm:"size:32;not null"`
	UseHash           string `gorm:"size:64;not null"`
	Epoch             string `gorm:"size:128;not null"`
	StorageGeneration string `gorm:"size:128;not null"`
	ServiceID         string `gorm:"size:128;not null"`
	InstanceID        string `gorm:"size:128;not null"`
	BuildSHA          string `gorm:"size:64;not null"`
	AuthorizationHash string `gorm:"size:64;not null"`
	State             string `gorm:"size:32;not null"`
	ContinuationState string `gorm:"size:32;not null"`
	StartedAtMs       int64  `gorm:"not null"`
	ReturnedAtMs      int64  `gorm:"not null"`
}

// RelayExecutionHandle cannot be decoded from JSON or recovered from journal
// hashes. Losing the process nonce leaves an active invocation for a future
// trusted executor fence/recovery protocol; it never authorizes replay.
type RelayExecutionHandle struct {
	store *ChannelAdmissionStore
	id    string
	nonce string
}

type RelayExecutionReturn string

const (
	RelayExecutionReturned      RelayExecutionReturn = "returned"
	RelayExecutionErrorReturned RelayExecutionReturn = "error_returned"
	RelayExecutionAsyncReturned RelayExecutionReturn = "async_returned"
)

func executionUseHash(use *ChannelAdmissionUse) string {
	hash, _ := admissionHash(
		[]any{
			use.ID,
			use.ResourceKey,
			use.RequestHash,
			use.Epoch,
			use.ChannelID,
			use.KeyDigest,
			use.IdentityHash,
			use.Lane,
			use.AdmittedAtMs,
		},
	)

	return hash
}

func executionAuthorization(row ChannelAdmissionExecution, nonce string) string {
	row.AuthorizationHash = ""
	row.State = "active"
	row.ReturnedAtMs = 0
	hash, _ := admissionHash([]any{"relay-execution-v1", row, nonce})

	return hash
}

// Explicit metadata queries distinguish an absent additive extension from a
// broken/incomplete installation. V1-only uses remain open and uncovered.
func channelExecutionStoragePresent(tx *gorm.DB) (bool, error) {
	var (
		count  int64
		result *gorm.DB
	)
	switch tx.Name() {
	case "sqlite":
		result = tx.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ?", []string{"channel_admission_executions", "channel_admission_execution_protocols"}).
			Scan(&count)
	case "postgres":
		result = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name IN ?", []string{"channel_admission_executions", "channel_admission_execution_protocols"}).
			Scan(&count)
	default:
		return false, ErrChannelAdmissionUnavailable
	}

	if result.Error != nil || count == 0 {
		return false, result.Error
	}

	if count != 2 {
		return false, ErrChannelAdmissionUnavailable
	}

	var protocol ChannelAdmissionExecutionProtocol
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&protocol, "id = ?", 1).
		Error; err != nil {
		return false, err
	}

	if protocol.Version != 1 {
		return false, ErrChannelAdmissionUnavailable
	}

	statement := &gorm.Statement{DB: tx}
	if err := statement.Parse(&ChannelAdmissionExecution{}); err != nil {
		return false, err
	}

	rows, err := tx.Model(&ChannelAdmissionExecution{}).
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

	if closeErr != nil {
		return false, closeErr
	}

	return true, nil
}

func (s *ChannelAdmissionStore) startRelayExecution(
	tx *gorm.DB,
	c *ChannelAdmissionControl,
	use *ChannelAdmissionUse,
	nonce string,
) (*RelayExecutionHandle, error) {
	present, err := channelExecutionStoragePresent(tx)
	if err != nil || !present {
		return nil, err
	}

	if use.Lane != "relay" || use.State != "open" || c.StorageGeneration == "" {
		return nil, ErrChannelAdmissionConflict
	}

	id, _ := admissionHash([]any{"relay-execution-id-v1", use.ID, nonce})

	now, err := channelAdmissionTime(tx)
	if err != nil {
		return nil, err
	}

	row := ChannelAdmissionExecution{
		ID:                id,
		UseID:             use.ID,
		Version:           1,
		Kind:              "relay_call",
		UseHash:           executionUseHash(use),
		Epoch:             c.Epoch,
		StorageGeneration: c.StorageGeneration,
		ServiceID:         s.Executor.ServiceID,
		InstanceID:        s.Executor.InstanceID,
		BuildSHA:          s.Executor.BuildSHA,
		State:             "active",
		ContinuationState: "unproven",
		StartedAtMs:       now.UnixMilli(),
	}

	row.AuthorizationHash = executionAuthorization(row, nonce)
	if err = tx.Create(&row).Error; err != nil {
		return nil, err
	}

	return &RelayExecutionHandle{store: s, id: id, nonce: nonce}, nil
}

// RecordReturn records that this invocation and its response-body cleanup have
// returned to its caller. An error/async return says nothing about upstream
// termination. Even a normal return does not settle billing or close the use.
// It deliberately does not depend on fresh admission proof: completion of the
// original invocation must survive a seal or proof expiry, but storage drift,
// replaced/missing facts or a different private capability are rejected.
//
//nolint:gocyclo // The return receipt must revalidate owner, use, proof, and current state after acquiring the same row locks.
func (h *RelayExecutionHandle) RecordReturn(ctx context.Context, state RelayExecutionReturn) error {
	if h == nil || h.store == nil || h.id == "" || h.nonce == "" {
		return ErrChannelAdmissionConflict
	}

	if state != RelayExecutionReturned && state != RelayExecutionErrorReturned &&
		state != RelayExecutionAsyncReturned {
		return ErrChannelAdmissionConflict
	}

	return h.store.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		present, err := channelExecutionStoragePresent(tx)
		if err != nil {
			return err
		}

		if !present {
			return ErrChannelAdmissionUnavailable
		}

		var row ChannelAdmissionExecution
		if err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&row, "id = ?", h.id).
			Error; err != nil {
			return err
		}

		if row.Version != 1 || row.Kind != "relay_call" || row.ContinuationState != "unproven" ||
			row.AuthorizationHash != executionAuthorization(row, h.nonce) ||
			row.StorageGeneration != c.StorageGeneration ||
			row.StartedAtMs <= 0 {
			return ErrChannelAdmissionConflict
		}

		var use ChannelAdmissionUse
		if err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&use, "id = ?", row.UseID).
			Error; err != nil {
			return err
		}

		if executionUseHash(&use) != row.UseHash || use.Lane != "relay" || use.State != "open" ||
			use.ClosedAtMs != 0 ||
			use.ClosureHash != "" {
			return ErrChannelAdmissionConflict
		}

		if row.State == string(state) && row.ReturnedAtMs >= row.StartedAtMs {
			return nil
		}

		if row.State != "active" || row.ReturnedAtMs != 0 {
			return ErrChannelAdmissionConflict
		}

		now, err := channelAdmissionTime(tx)
		if err != nil {
			return err
		}

		if now.UnixMilli() < row.StartedAtMs {
			return ErrChannelAdmissionConflict
		}

		result := tx.Model(&ChannelAdmissionExecution{}).
			Where("id = ? AND state = ? AND returned_at_ms = ? AND authorization_hash = ?", row.ID, "active", 0, row.AuthorizationHash).
			Updates(map[string]any{"state": string(state), "returned_at_ms": now.UnixMilli()})
		if result.Error != nil {
			return result.Error
		}

		if result.RowsAffected != 1 {
			return ErrChannelAdmissionConflict
		}

		return nil
	})
}
