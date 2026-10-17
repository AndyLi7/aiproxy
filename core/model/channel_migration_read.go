package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

const MigrationReadSource = "gateway-migration-read-v1"

// These HTTP DTOs contain only typed facts, never raw channel/config data or
// credentials. Every int64 revision/time uses decimal strings across JS's boundary.
type MigrationReadResult struct {
	Version           int                    `json:"version"`
	Source            string                 `json:"source"`
	State             string                 `json:"state"`
	Reason            string                 `json:"reason"`
	Protocol          *MigrationReadProtocol `json:"protocol,omitempty"`
	ExecutionReady    *bool                  `json:"executionReady,omitempty"`
	Blockers          []string               `json:"blockers,omitempty"`
	Snapshot          *MigrationReadSnapshot `json:"snapshot,omitempty"`
	Readback          *MigrationReadback     `json:"readback,omitempty"`
	OperationID       string                 `json:"operationId,omitempty"`
	Epoch             string                 `json:"epoch,omitempty"`
	Revision          string                 `json:"revision,omitempty"`
	StorageGeneration string                 `json:"storageGeneration,omitempty"`
	ConfigModels      *[]string              `json:"configModels,omitempty"`
}
type MigrationReadProtocol struct {
	Version           int    `json:"version"`
	AtomicPins        bool   `json:"atomicPins"`
	CurrentHeadRead   bool   `json:"currentHeadRead"`
	ForwardOnly       bool   `json:"forwardOnly"`
	StorageGeneration string `json:"storageGeneration"`
}
type MigrationReadSnapshot struct {
	Epoch             string   `json:"epoch"`
	Revision          string   `json:"revision"`
	StorageGeneration string   `json:"storageGeneration"`
	ConfigModels      []string `json:"configModels"`
	PinHash           string   `json:"pinHash"`
	ProjectionHash    string   `json:"projectionHash"`
	FamilyHash        string   `json:"familyHash"`
}
type MigrationReadSealRequest struct {
	OperationID      string          `json:"operationId"`
	Epoch            string          `json:"epoch"`
	ExpectedRevision string          `json:"expectedRevision"`
	From             ChannelIdentity `json:"from"`
	To               ChannelIdentity `json:"to"`
	RevertOf         string          `json:"revertOf"`
}
type MigrationReadRequest struct {
	TargetResourceKind string                   `json:"targetResourceKind,omitempty"`
	Seal               MigrationReadSealRequest `json:"seal"`
	FromCredentialID   string                   `json:"fromCredentialId"`
	ToCredentialID     string                   `json:"toCredentialId"`
	ExpectedPinHash    string                   `json:"expectedPinHash"`
	ProjectionHash     string                   `json:"projectionHash"`
	FamilyHash         string                   `json:"familyHash"`
}
type MigrationReadReceipt struct {
	OperationID       string `json:"operationId"`
	RequestHash       string `json:"requestHash"`
	StorageGeneration string `json:"storageGeneration"`
	ProbeHash         string `json:"probeHash"`
	ProbeFactHash     string `json:"probeFactHash"`
	State             string `json:"state"`
	SealRevision      string `json:"sealRevision"`
	CommitRevision    string `json:"commitRevision"`
	CommittedAtMs     string `json:"committedAtMs"`
	AfterPinHash      string `json:"afterPinHash"`
	FromChannelID     int    `json:"fromChannelId"`
	ToChannelID       int    `json:"toChannelId"`
}
type MigrationReadHead struct {
	Version          int    `json:"version"`
	OperationID      string `json:"operationId"`
	Epoch            string `json:"epoch"`
	Revision         string `json:"revision"`
	FromIdentityHash string `json:"fromIdentityHash"`
	ToIdentityHash   string `json:"toIdentityHash"`
	RecordedAtMs     string `json:"recordedAtMs"`
}
type MigrationReadback struct {
	Request        MigrationReadRequest `json:"request"`
	Receipt        MigrationReadReceipt `json:"receipt"`
	From           ChannelIdentity      `json:"from"`
	To             ChannelIdentity      `json:"to"`
	FromEnabled    bool                 `json:"fromEnabled"`
	ToEnabled      bool                 `json:"toEnabled"`
	PinHash        string               `json:"pinHash"`
	ProjectionHash string               `json:"projectionHash"`
	FamilyHash     string               `json:"familyHash"`
	// CurrentHead is this operation's seal head; prepared may still have prior family history.
	CurrentHead *MigrationReadHead `json:"currentHead"`
}

func migrationReadState(state, reason string) MigrationReadResult {
	return MigrationReadResult{Version: 1, Source: MigrationReadSource, State: state, Reason: reason}
}
func migrationDecimal(v int64) string { return strconv.FormatInt(v, 10) }

// No revision=revision lock write: PG uses a consistent snapshot plus control
// SHARE (all command/normal writers acquire control first); SQLite uses its
// transaction read snapshot. Unknown schemas never produce authoritative absence.
func (s *ChannelAdmissionStore) migrationReadTransaction(ctx context.Context, work func(*gorm.DB, *ChannelAdmissionControl) (MigrationReadResult, error)) MigrationReadResult {
	unsupported := migrationReadState("unsupported", "migration_read_storage_unavailable")
	if s == nil || s.DB == nil || s.DB != s.LogDB {
		return unsupported
	}
	dialect := s.DB.Dialector.Name()
	if dialect != "sqlite" && dialect != "postgres" {
		return unsupported
	}
	options := &sql.TxOptions{}
	if dialect == "postgres" {
		options.Isolation = sql.LevelRepeatableRead
	}
	result := unsupported
	storageReady := false
	err := s.DB.WithContext(ctx).Session(&gorm.Session{Logger: logger.Discard}).Transaction(func(tx *gorm.DB) error {
		if !ChannelAdmissionStorageReady(tx) {
			return ErrChannelAdmissionUnavailable
		}
		q := tx
		if dialect == "postgres" {
			q = q.Clauses(clause.Locking{Strength: "SHARE"})
		}
		var c ChannelAdmissionControl
		if err := q.First(&c, 1).Error; err != nil {
			return err
		}
		if c.Version != ChannelAdmissionProtocolVersion || c.Epoch == "" || c.StorageGeneration == "" || c.EpochSequence <= 0 || c.ActiveProofID == "" || c.Revision < 0 {
			return ErrChannelAdmissionUnavailable
		}
		if err := migrationStorageReady(tx); err != nil {
			return err
		}
		// A complete model_config table and both channel identity columns are also
		// prerequisites for an authoritative missing operation, not just its table.
		for _, m := range []any{&Channel{}, &ModelConfig{}} {
			st := &gorm.Statement{DB: tx}
			if err := st.Parse(m); err != nil {
				return err
			}
			rows, err := tx.Model(m).Select(st.Schema.DBNames).Limit(0).Rows()
			if err != nil {
				return ErrChannelAdmissionUnavailable
			}
			err = rows.Err()
			closeErr := rows.Close()
			if err != nil || closeErr != nil {
				return ErrChannelAdmissionUnavailable
			}
		}
		if err := migrationReadBoundSource(tx); err != nil {
			return err
		}
		storageReady = true
		var err error
		result, err = work(tx, &c)
		return err
	}, options)
	if err != nil {
		if !storageReady && errors.Is(err, gorm.ErrRecordNotFound) {
			return unsupported
		}
		if errors.Is(err, ErrChannelAdmissionConflict) || errors.Is(err, gorm.ErrRecordNotFound) {
			return migrationReadState("conflict", "migration_read_conflict")
		}
		if errors.Is(err, ErrChannelAdmissionUnavailable) && !storageReady {
			return unsupported
		}
		return migrationReadState("unavailable", "migration_read_unavailable")
	}
	return result
}

func (s *ChannelAdmissionStore) ReadMigrationCommandProtocol(ctx context.Context) MigrationReadResult {
	return s.migrationReadTransaction(ctx, func(_ *gorm.DB, c *ChannelAdmissionControl) (MigrationReadResult, error) {
		r := migrationReadState("ready", "")
		disabled := false
		r.ExecutionReady = &disabled
		r.Protocol = &MigrationReadProtocol{1, true, true, true, c.StorageGeneration}
		r.Blockers = []string{"trusted_deployment_source_not_connected", "restricted_probe_producer_not_connected", "safe_sealed_recovery_not_connected", "joint_drain_proof_not_connected"}
		return r, nil
	})
}
func (s *ChannelAdmissionStore) ReadMigrationSnapshotFacts(ctx context.Context, from, to ChannelIdentity) MigrationReadResult {
	if !validChannelIdentity(from) || !validChannelIdentity(to) || from.ChannelID == to.ChannelID {
		return migrationReadState("conflict", "migration_read_invalid_identity")
	}
	return s.migrationReadTransaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) (MigrationReadResult, error) {
		snap, err := readMigrationSnapshotTx(tx, c, from, to, true)
		if err != nil {
			return MigrationReadResult{}, err
		}
		r := migrationReadState("ready", "")
		r.Snapshot = migrationSnapshotDTO(snap)
		return r, nil
	})
}
func migrationSnapshotDTO(s ChannelMigrationSnapshot) *MigrationReadSnapshot {
	return &MigrationReadSnapshot{s.Epoch, migrationDecimal(s.Revision), s.StorageGeneration, s.ConfigModels, s.PinHash, s.ProjectionHash, s.FamilyHash}
}

func (s *ChannelAdmissionStore) ReadPreparedMigrationFacts(ctx context.Context, operationID string) MigrationReadResult {
	if !ValidMigrationReadOperationID(operationID) {
		return migrationReadState("conflict", "migration_read_invalid_operation")
	}
	return s.migrationReadTransaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) (MigrationReadResult, error) {
		r := migrationReadState("ready", "")
		r.OperationID = operationID
		r.Epoch = c.Epoch
		r.Revision = migrationDecimal(c.Revision)
		r.StorageGeneration = c.StorageGeneration
		// Only this exact lookup can establish absent. Missing rows in validators
		// (probe, barrier, family, channel) are conflicts/unavailable, never absence.
		var count int64
		if err := tx.Model(&ChannelMigrationPrepared{}).Where("operation_id = ?", operationID).Count(&count).Error; err != nil {
			return r, err
		}
		if count == 0 {
			if err := migrationReadNoOperationEffects(tx, operationID); err != nil {
				return r, err
			}
			r.State = "absent"
			return r, nil
		}
		row, err := readPreparedMigrationTx(tx, c, operationID, true)
		if err != nil {
			return r, err
		}
		if row.State == "prepared" {
			if err := migrationReadNoOperationEffects(tx, operationID); err != nil {
				return r, err
			}
		}
		if row.State == "abandoned" {
			return r, ErrChannelAdmissionConflict
		}
		var req ChannelMigrationRequest
		if json.Unmarshal([]byte(row.RequestJSON), &req) != nil {
			return r, ErrChannelAdmissionConflict
		}
		if req.Seal.ExpectedRevision < 0 || req.Seal.ExpectedRevision > c.Revision {
			return r, ErrChannelAdmissionConflict
		}
		snap, err := readMigrationSnapshotTx(tx, c, req.Seal.From, req.Seal.To, true)
		if err != nil {
			return r, err
		}
		from, err := storedAdmissionIdentity(tx, req.Seal.From, true)
		if err != nil {
			return r, err
		}
		to, err := storedAdmissionIdentity(tx, req.Seal.To, true)
		if err != nil {
			return r, err
		}
		r.ConfigModels = &snap.ConfigModels
		r.Readback = &MigrationReadback{
			Request: MigrationReadRequest{req.TargetResourceKind, MigrationReadSealRequest{req.Seal.OperationID, req.Seal.Epoch, migrationDecimal(req.Seal.ExpectedRevision), req.Seal.From, req.Seal.To, req.Seal.RevertOf}, req.FromCredentialID, req.ToCredentialID, req.ExpectedPinHash, req.ProjectionHash, req.FamilyHash},
			Receipt: MigrationReadReceipt{row.OperationID, row.RequestHash, row.StorageGeneration, row.ProbeHash, row.ProbeFactHash, row.State, migrationDecimal(row.SealRevision), migrationDecimal(row.CommitRevision), migrationDecimal(row.CommittedAtMs), row.AfterPinHash, row.FromChannelID, row.ToChannelID},
			From:    req.Seal.From, To: req.Seal.To, FromEnabled: from.Status == ChannelStatusEnabled, ToEnabled: to.Status == ChannelStatusEnabled, PinHash: snap.PinHash, ProjectionHash: snap.ProjectionHash, FamilyHash: snap.FamilyHash,
		}
		if row.State != "prepared" {
			var head ChannelAdmissionBarrier
			if err := tx.First(&head, "operation_id = ?", operationID).Error; err != nil {
				return r, err
			}
			if head.RecordedAtMs <= 0 || head.Revision > c.Revision || row.CommitRevision > c.Revision {
				return r, ErrChannelAdmissionConflict
			}
			r.Readback.CurrentHead = &MigrationReadHead{head.Version, head.OperationID, head.Epoch, migrationDecimal(head.Revision), head.FromIdentityHash, head.ToIdentityHash, migrationDecimal(head.RecordedAtMs)}
		}
		return r, nil
	})
}
func ValidMigrationReadOperationID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	first := id[0]
	if !(first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z' || first >= '0' && first <= '9') {
		return false
	}
	for _, c := range []byte(id) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}

func migrationReadNoOperationEffects(tx *gorm.DB, id string) error {
	for _, m := range []any{&ChannelAdmissionBarrier{}, &ChannelAdmissionFamily{}} {
		var n int64
		if err := tx.Model(m).Where("operation_id = ?", id).Count(&n).Error; err != nil {
			return err
		}
		if n != 0 {
			return ErrChannelAdmissionConflict
		}
	}
	return nil
}

// Bounds precede recursive legacy command validators and GORM JSON decoding.
// These rows are hashed/validated only; their contents never enter a DTO/log.
func migrationReadBoundSource(tx *gorm.DB) error {
	total := 0
	for _, spec := range []struct {
		table       string
		columns     []string
		jsonColumns []string
	}{
		{"channels", []string{"id", "configs"}, []string{"configs"}},
		{"model_configs", nil, []string{"config", "plugin"}},
	} {
		query := tx.Table(spec.table).Limit(MigrationInventoryMaxRows + 1)
		if len(spec.columns) > 0 {
			query = query.Select(spec.columns)
		}
		rows, err := migrationRawRows(query)
		if err != nil {
			return err
		}
		if len(rows) > MigrationInventoryMaxRows {
			return ErrChannelAdmissionUnavailable
		}
		for _, row := range rows {
			for _, value := range row {
				if text, ok := value.(string); ok {
					total += len(text)
					if total > MigrationInventoryMaxBytes {
						return ErrChannelAdmissionUnavailable
					}
				}
			}
			for _, column := range spec.jsonColumns {
				if value := row[column]; value != nil && value != "" {
					raw, ok := value.(string)
					if !ok {
						return ErrChannelAdmissionUnavailable
					}
					if _, ok = inventoryDecode([]byte(raw)); !ok {
						return ErrChannelAdmissionUnavailable
					}
				}
			}
		}
	}
	return nil
}
