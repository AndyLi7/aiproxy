package model

// These are server-only commands. No controller decodes proof facts from JSON.
// A real restricted probe/issuer adapter is a separate prerequisite, not a
// boolean supplied by an administrator. Installing this schema enables nothing.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ChannelMigrationProtocol struct {
	ID      int `gorm:"primaryKey"`
	Version int `gorm:"not null"`
}

// Installed/revoked only by the future trusted probe updater, never by these commands.
// This independently locked row makes revocation visible during channel lock waits.
type ChannelMigrationProbeRecord struct {
	EvidenceHash string `gorm:"primaryKey;size:64"`
	FactHash     string `gorm:"size:64;not null"`
	Revoked      bool   `gorm:"not null"`
}

type ChannelMigrationPrepared struct {
	ShadowCheckpointHash string `gorm:"size:64;not null;default:''"`
	OperationID          string `gorm:"primaryKey;size:128"`
	RequestJSON          string `gorm:"type:text;not null"`
	RequestHash          string `gorm:"size:64;not null"`
	StorageGeneration    string `gorm:"size:128;not null"`
	ProbeHash            string `gorm:"size:64;not null"`
	ProbeFactHash        string `gorm:"size:64;not null"`
	State                string `gorm:"size:16;not null"`
	SealRevision         int64  `gorm:"not null"`
	CommitRevision       int64  `gorm:"not null"`
	CommittedAtMs        int64  `gorm:"not null"`
	AfterPinHash         string `gorm:"size:64;not null"`
	FromChannelID        int    `gorm:"index;not null"`
	ToChannelID          int    `gorm:"index;not null"`
}

// ExpectedPinHash is a gateway-generated opaque digest of the complete raw
// model_config table, NOT the application's normalized migrationPinSetHash.
// This avoids lossy float64/JS-number or default/omitempty round trips.
// Empty TargetResourceKind preserves the original forward JSON/hash. Reverse
// requires the dedicated retired-target resource and server-only commands.
type ChannelMigrationRequest struct {
	TargetResourceKind string             `json:"targetResourceKind,omitempty"`
	Seal               ChannelSealRequest `json:"seal"`
	FromCredentialID   string             `json:"fromCredentialId"`
	ToCredentialID     string             `json:"toCredentialId"`
	ExpectedPinHash    string             `json:"expectedPinHash"`
	ProjectionHash     string             `json:"projectionHash"`
	FamilyHash         string             `json:"familyHash"`
}

// The verifier must validate durable restricted-probe execution/settlement,
// exact operation/request/physical identities and current revocation state.
// Network work runs before the transaction. EvidenceHash identifies immutable
// evidence, not "passed"; time validity is checked again after all writes.
type VerifiedMigrationProbe struct {
	RequestHash       string
	EvidenceHash      string
	StorageGeneration string
	IssuedAtMs        int64
	ValidUntilMs      int64
}

type migrationAuthorization struct {
	request    ChannelMigrationRequest
	hash       string
	probe      VerifiedMigrationProbe
	deployment VerifiedDeploymentProof
}

type ChannelMigrationSnapshot struct {
	Epoch             string
	Revision          int64
	StorageGeneration string
	ConfigModels      []string
	PinHash           string
	ProjectionHash    string
	FamilyHash        string
}

// ReadMigrationSnapshot returns hashes only, never channel credentials/raw rows.
func (s *ChannelAdmissionStore) ReadMigrationSnapshot(ctx context.Context, from, shadow ChannelIdentity) (result ChannelMigrationSnapshot, err error) {
	err = s.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		result, err = readMigrationSnapshotTx(tx, c, from, shadow, false)
		return err
	})
	return
}

func readMigrationSnapshotTx(tx *gorm.DB, c *ChannelAdmissionControl, from, shadow ChannelIdentity, snapshotRead bool) (result ChannelMigrationSnapshot, err error) {
	err = func() error {
		if _, err := storedAdmissionIdentity(tx, from, snapshotRead); err != nil {
			return err
		}
		result.ConfigModels = []string{}
		result.Epoch = c.Epoch
		result.Revision = c.Revision
		result.StorageGeneration = c.StorageGeneration
		if _, err := storedAdmissionIdentity(tx, shadow, snapshotRead); err != nil {
			return err
		}
		rows, err := migrationConfigRows(tx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			name, ok := row["model"].(string)
			if !ok {
				return ErrChannelAdmissionConflict
			}
			result.ConfigModels = append(result.ConfigModels, name)
		}
		result.PinHash, err = admissionHash(rows)
		if err != nil {
			return err
		}
		result.ProjectionHash, err = migrationProjection(tx, shadow.ChannelID)
		if err != nil {
			return err
		}
		result.FamilyHash, err = migrationFamilyHash(tx, from.KeyDigest, shadow.KeyDigest)
		return err
	}()
	return
}

func migrationStorageReady(tx *gorm.DB) error {
	for _, m := range []any{&ChannelMigrationProtocol{}, &ChannelMigrationPrepared{}, &ChannelMigrationProbeRecord{}} {
		st := &gorm.Statement{DB: tx}
		if err := st.Parse(m); err != nil {
			return err
		}
		columns := st.Schema.DBNames
		if _, isPrepared := m.(*ChannelMigrationPrepared); isPrepared {
			present, e := shadowStoragePresent(tx)
			if e != nil {
				return e
			}
			if !present {
				columns = make([]string, 0, len(st.Schema.DBNames))
				for _, name := range st.Schema.DBNames {
					if name != "shadow_checkpoint_hash" {
						columns = append(columns, name)
					}
				}
			}
		}
		rows, err := tx.Model(m).Select(columns).Limit(0).Rows()
		if err != nil {
			return ErrChannelAdmissionUnavailable
		}
		e := rows.Err()
		ce := rows.Close()
		if e != nil || ce != nil {
			return ErrChannelAdmissionUnavailable
		}
	}
	var protocol ChannelMigrationProtocol
	if tx.First(&protocol, 1).Error != nil || protocol.Version != 1 {
		return ErrChannelAdmissionUnavailable
	}
	if err := optionalShadowStorageReady(tx); err != nil {
		return err
	}
	return migrationStatesReady(tx)
}
func (s *ChannelAdmissionStore) migrationAuthorization(ctx context.Context, input ChannelMigrationRequest) (*migrationAuthorization, error) {
	if s != nil && s.controlledRuntime {
		return nil, ErrChannelAdmissionUnavailable
	}
	if s == nil || s.LoadMigrationProbe == nil || !validMigrationResourceKind(input) || input.FromCredentialID == "" || input.ToCredentialID == "" || len(input.FromCredentialID) > 128 || len(input.ToCredentialID) > 128 || !fullIdentityDigest(input.ExpectedPinHash) || !fullIdentityDigest(input.ProjectionHash) || !fullIdentityDigest(input.FamilyHash) ||
		(input.Seal.From.KeyDigest == input.Seal.To.KeyDigest) != (input.FromCredentialID == input.ToCredentialID) {
		return nil, ErrChannelAdmissionUnavailable
	}
	hash, err := admissionHash(input)
	if err != nil {
		return nil, err
	}
	deployment, err := s.deployment(ctx)
	if err != nil {
		return nil, err
	}
	probe, err := s.LoadMigrationProbe(ctx, input)
	if err != nil {
		return nil, err
	}
	if probe.RequestHash != hash || !fullIdentityDigest(probe.EvidenceHash) || probe.StorageGeneration != deployment.StorageGeneration || probe.IssuedAtMs <= 0 || probe.ValidUntilMs <= probe.IssuedAtMs {
		return nil, ErrChannelAdmissionUnavailable
	}
	return &migrationAuthorization{input, hash, probe, deployment}, nil
}
func (p VerifiedMigrationProbe) Fingerprint() (string, error) { return admissionHash(p) }

func finalizeMigrationProbe(tx *gorm.DB, p VerifiedMigrationProbe) error {
	hash, err := p.Fingerprint()
	if err != nil {
		return err
	}
	var stored ChannelMigrationProbeRecord
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&stored, "evidence_hash = ?", p.EvidenceHash).Error; err != nil {
		return err
	}
	if stored.Revoked || stored.FactHash != hash {
		return ErrChannelAdmissionUnavailable
	}
	now, err := channelAdmissionTime(tx)
	if err != nil {
		return err
	}
	if now.UnixMilli() < p.IssuedAtMs || now.UnixMilli() >= p.ValidUntilMs {
		return ErrChannelAdmissionUnavailable
	}
	return nil
}
func loadMigration(tx *gorm.DB, id string, snapshotRead ...bool) (*ChannelMigrationPrepared, error) {
	if err := migrationStorageReady(tx); err != nil {
		return nil, err
	}
	var row ChannelMigrationPrepared
	q := tx
	if len(snapshotRead) == 0 || !snapshotRead[0] {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := q.First(&row, "operation_id = ?", id).Error; err != nil {
		return nil, err
	}
	var request ChannelMigrationRequest
	if json.Unmarshal([]byte(row.RequestJSON), &request) != nil {
		return nil, ErrChannelAdmissionConflict
	}
	hash, _ := admissionHash(request)
	if !validMigrationResourceKind(request) || row.RequestHash != hash || row.OperationID != request.Seal.OperationID || row.FromChannelID != request.Seal.From.ChannelID || row.ToChannelID != request.Seal.To.ChannelID || !fullIdentityDigest(row.ProbeHash) || !fullIdentityDigest(row.ProbeFactHash) || row.StorageGeneration == "" {
		return nil, ErrChannelAdmissionConflict
	}
	switch row.State {
	case "prepared", "abandoned":
		if row.SealRevision != 0 || row.CommitRevision != 0 || row.CommittedAtMs != 0 || row.AfterPinHash != "" {
			return nil, ErrChannelAdmissionConflict
		}
	case "sealed":
		if row.SealRevision <= 0 || row.CommitRevision != 0 || row.CommittedAtMs != 0 || row.AfterPinHash != "" {
			return nil, ErrChannelAdmissionConflict
		}
	case "committed":
		if row.SealRevision <= 0 || row.CommitRevision <= row.SealRevision || row.CommittedAtMs <= 0 || !fullIdentityDigest(row.AfterPinHash) {
			return nil, ErrChannelAdmissionConflict
		}
	default:
		return nil, ErrChannelAdmissionConflict
	}
	return &row, nil
}
func validateMigrationAuthorization(tx *gorm.DB, c *ChannelAdmissionControl, a *migrationAuthorization, stage string, features []string) error {
	// All command writers already hold control; acquire owned resource before Prepared.
	if err := lockMigrationTarget(tx, a.request); err != nil {
		return err
	}
	row, err := loadMigration(tx, a.request.Seal.OperationID)
	if err != nil {
		return err
	}
	if err := validatePreparedTargetLink(tx, c, row, a.request, false); err != nil {
		return err
	}
	encoded, _ := json.Marshal(a.request)
	probeFactHash, _ := a.probe.Fingerprint()
	if a.request.Seal.Epoch != c.Epoch || row.RequestHash != a.hash || row.RequestJSON != string(encoded) || row.StorageGeneration != c.StorageGeneration || row.ProbeHash != a.probe.EvidenceHash || row.ProbeFactHash != probeFactHash || row.FromChannelID != a.request.Seal.From.ChannelID || row.ToChannelID != a.request.Seal.To.ChannelID {
		return ErrChannelAdmissionConflict
	}
	if stage == "seal" && row.State != "prepared" && row.State != "sealed" {
		return ErrChannelAdmissionConflict
	}
	if stage == "commit" && row.State != "sealed" && row.State != "committed" {
		return ErrChannelAdmissionConflict
	}
	if row.State != "prepared" {
		if err := migrationCurrentHead(tx, row, a.request); err != nil {
			return err
		}
	}
	if stage == "seal" {
		if row.State == "prepared" && reverseMigration(a.request) {
			r, e := loadRetiredResource(tx, row.OperationID, false)
			if e != nil {
				return e
			}
			if e = retiredCurrent(tx, c, r, a.request, features); e != nil {
				return e
			}
		} else if row.State == "prepared" && row.ShadowCheckpointHash != "" {
			r, e := loadShadowResource(tx, row.OperationID, row.ToChannelID, false)
			if e != nil || r == nil {
				return ErrChannelAdmissionConflict
			}
			if e = lockShadowSourceRows(tx); e != nil {
				return e
			}
			if _, e = shadowObserve(tx, c, r, features, true); e != nil {
				return e
			}
		}
		if row.State == "prepared" {
			family, err := migrationFamilyHash(tx, a.request.Seal.From.KeyDigest, a.request.Seal.To.KeyDigest)
			if err != nil {
				return err
			}
			if family != a.request.FamilyHash {
				return ErrChannelAdmissionConflict
			}
		}
		if err := migrationSnapshotMatches(tx, a.request, migrationTargetStatus(a.request)); err != nil {
			return err
		}
	}
	if stage == "seal" {
		return finalizeMigrationProbe(tx, a.probe)
	}
	return nil
}

// PrepareMigration freezes the reviewed snapshot only after a server-side probe
// verifier binds its evidence to this exact request. It never enables a channel.
func (s *ChannelAdmissionStore) PrepareMigration(ctx context.Context, input ChannelMigrationRequest) (result *ChannelMigrationPrepared, err error) {
	if input.TargetResourceKind != "" || input.Seal.RevertOf != "" {
		return nil, ErrChannelAdmissionConflict
	}
	a, err := s.migrationAuthorization(ctx, input)
	if err != nil {
		return nil, err
	}
	if input.Seal.OperationID == "" || len(input.Seal.OperationID) > 128 || !validChannelIdentity(input.Seal.From) || !validChannelIdentity(input.Seal.To) || input.Seal.From.ChannelID == input.Seal.To.ChannelID {
		return nil, ErrChannelAdmissionConflict
	}
	err = s.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		if err := migrationStorageReady(tx); err != nil {
			return err
		}
		resource, err := prepareShadowResource(tx, c, input, s.MigrationFeatures)
		if err != nil {
			return err
		}
		var old ChannelMigrationPrepared
		e := tx.First(&old, "operation_id = ?", input.Seal.OperationID).Error
		if e == nil {
			if err := validateMigrationAuthorization(tx, c, a, "seal", s.MigrationFeatures); err != nil {
				return err
			}
			if _, err := lockDeploymentProof(tx, c, a.deployment, s.Executor); err != nil {
				return err
			}
			result = &old
			return finalizeMigrationProbe(tx, a.probe)
		}
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		if input.Seal.ExpectedRevision < 0 || c.Revision < input.Seal.ExpectedRevision || c.Epoch != input.Seal.Epoch {
			return ErrChannelAdmissionConflict
		}
		family, err := migrationFamilyHash(tx, input.Seal.From.KeyDigest, input.Seal.To.KeyDigest)
		if err != nil {
			return err
		}
		if family != input.FamilyHash {
			return ErrChannelAdmissionConflict
		}
		if err := migrationSnapshotMatches(tx, input, migrationTargetStatus(input)); err != nil {
			return err
		}
		if _, err := lockDeploymentProof(tx, c, a.deployment, s.Executor); err != nil {
			return err
		}
		// A physical shadow cannot be owned by two simultaneous preparations.
		var n int64
		if err := tx.Model(&ChannelMigrationPrepared{}).Where("state IN ? AND (to_channel_id IN ? OR from_channel_id IN ?)", []string{"prepared", "sealed"}, []int{input.Seal.From.ChannelID, input.Seal.To.ChannelID}, []int{input.Seal.From.ChannelID, input.Seal.To.ChannelID}).Count(&n).Error; err != nil {
			return err
		}
		if n != 0 {
			return ErrChannelAdmissionConflict
		}
		raw, _ := json.Marshal(input)
		probeFactHash, _ := a.probe.Fingerprint()
		row := ChannelMigrationPrepared{OperationID: input.Seal.OperationID, RequestJSON: string(raw), RequestHash: a.hash, StorageGeneration: c.StorageGeneration, ProbeHash: a.probe.EvidenceHash, ProbeFactHash: probeFactHash, State: "prepared", FromChannelID: input.Seal.From.ChannelID, ToChannelID: input.Seal.To.ChannelID}
		if resource != nil {
			row.ShadowCheckpointHash = resource.CheckpointHash
		}
		create := tx
		if resource == nil {
			create = create.Omit("ShadowCheckpointHash")
		}
		if err := create.Create(&row).Error; err != nil {
			return err
		}
		if resource != nil {
			if err := consumeShadowResource(tx, c, resource, &row); err != nil {
				return err
			}
		}
		if err := finalizeMigrationProbe(tx, a.probe); err != nil {
			return err
		}
		if err := finalizeAdmissionProof(tx, c, a.deployment, c.Revision); err != nil {
			return err
		}
		if _, err := lockDeploymentProof(tx, c, a.deployment, s.Executor); err != nil {
			return err
		}
		if err := finalizeMigrationProbe(tx, a.probe); err != nil {
			return err
		}
		result = &row
		return nil
	})
	return
}
func (s *ChannelAdmissionStore) SealPrepared(ctx context.Context, input ChannelMigrationRequest) (*ChannelAdmissionBarrier, error) {
	if input.TargetResourceKind != "" || input.Seal.RevertOf != "" {
		return nil, ErrChannelAdmissionConflict
	}
	a, err := s.migrationAuthorization(ctx, input)
	if err != nil {
		return nil, err
	}
	return s.seal(ctx, input.Seal, a)
}
func migrationSnapshotMatches(tx *gorm.DB, input ChannelMigrationRequest, toStatus int, snapshotRead ...bool) error {
	from, err := storedAdmissionIdentity(tx, input.Seal.From, snapshotRead...)
	if err != nil {
		return err
	}
	to, err := storedAdmissionIdentity(tx, input.Seal.To, snapshotRead...)
	if err != nil {
		return err
	}
	if from.Status != ChannelStatusEnabled || to.Status != toStatus {
		return ErrChannelAdmissionConflict
	}
	projection, err := migrationProjection(tx, to.ID)
	if err != nil {
		return err
	}
	if projection != input.ProjectionHash {
		return ErrChannelAdmissionConflict
	}
	rows, err := migrationConfigRows(tx)
	if err != nil {
		return err
	}
	hash, err := admissionHash(rows)
	if err != nil {
		return err
	}
	if hash != input.ExpectedPinHash {
		return ErrChannelAdmissionConflict
	}
	// Do not seal admission for a snapshot that is already known to be
	// uncommittable. Prepare and Seal run the exact same complete rewrite plan
	// as Commit under the control lock, before any barrier or family change.
	_, err = migrationPlanConfigRewrites(rows, input)
	return err
}
func migrationCurrentHead(tx *gorm.DB, row *ChannelMigrationPrepared, input ChannelMigrationRequest) error {
	if row.SealRevision <= 0 {
		return ErrChannelAdmissionConflict
	}
	var barrier ChannelAdmissionBarrier
	if err := tx.First(&barrier, "operation_id = ?", row.OperationID).Error; err != nil {
		return err
	}
	wantHash, _ := admissionHash(input.Seal)
	if barrier.Version != ChannelAdmissionProtocolVersion || barrier.RequestHash != wantHash || barrier.Epoch != input.Seal.Epoch || barrier.FromIdentityHash != input.Seal.From.IdentityHash || barrier.ToIdentityHash != input.Seal.To.IdentityHash || barrier.FromKeyDigest != input.Seal.From.KeyDigest || barrier.ToKeyDigest != input.Seal.To.KeyDigest || barrier.Revision != row.SealRevision || barrier.FromChannelID != input.Seal.From.ChannelID || barrier.ToChannelID != input.Seal.To.ChannelID {
		return ErrChannelAdmissionConflict
	}
	for _, d := range []string{input.Seal.From.KeyDigest, input.Seal.To.KeyDigest} {
		family, err := admissionFamily(tx, d)
		if err != nil {
			return err
		}
		if family == nil || family.OperationID != row.OperationID || family.Revision != row.SealRevision {
			return ErrChannelAdmissionConflict
		}
		expected := 0
		identity := ""
		if d == input.Seal.To.KeyDigest {
			expected = input.Seal.To.ChannelID
			identity = input.Seal.To.IdentityHash
		}
		if family.AllowedChannelID != expected || family.AllowedIdentityHash != identity {
			return ErrChannelAdmissionConflict
		}
	}
	return nil
}

// CommitPreparedPins has NO separate enable operation: channel enabling, full
// snapshot CAS, raw JSON pin edits and the durable receipt commit atomically.
// Unknown outcomes are resolved by ReadPreparedMigration, never by status alone.
func (s *ChannelAdmissionStore) CommitPreparedPins(ctx context.Context, input ChannelMigrationRequest, sealRevision int64) (result *ChannelMigrationPrepared, err error) {
	if input.TargetResourceKind != "" || input.Seal.RevertOf != "" {
		return nil, ErrChannelAdmissionConflict
	}
	a, err := s.migrationAuthorization(ctx, input)
	if err != nil {
		return nil, err
	}
	err = s.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		result, err = s.commitPreparedPinsTx(tx, c, a, sealRevision)
		return err
	})
	if err == nil && s.DB == DB {
		rebuildModelCachesAfterWrite()
	}
	return
}
func migrationCommittedMatches(tx *gorm.DB, row *ChannelMigrationPrepared, input ChannelMigrationRequest, snapshotRead ...bool) error {
	if row.State != "committed" || row.CommitRevision <= row.SealRevision || row.CommittedAtMs <= 0 || !fullIdentityDigest(row.AfterPinHash) {
		return ErrChannelAdmissionConflict
	}
	copy := input
	copy.ExpectedPinHash = row.AfterPinHash
	return migrationSnapshotMatches(tx, copy, ChannelStatusEnabled, snapshotRead...)
}

// ReadPreparedMigration requires the CURRENT family head and, for committed
// state, every atomic postcondition. Historical receipts cannot authorize work.
func (s *ChannelAdmissionStore) ReadPreparedMigration(ctx context.Context, operationID string) (result *ChannelMigrationPrepared, err error) {
	err = s.controlTransaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		result, err = readPreparedMigrationTx(tx, c, operationID, false)
		return err
	}, false, true)
	return
}

func readPreparedMigrationTx(tx *gorm.DB, c *ChannelAdmissionControl, operationID string, snapshotRead bool) (result *ChannelMigrationPrepared, err error) {
	err = func() error {
		row, err := loadMigration(tx, operationID, snapshotRead)
		if err != nil {
			return err
		}
		var req ChannelMigrationRequest
		if json.Unmarshal([]byte(row.RequestJSON), &req) != nil {
			return ErrChannelAdmissionConflict
		}
		hash, _ := admissionHash(req)
		if hash != row.RequestHash || row.StorageGeneration != c.StorageGeneration || req.Seal.Epoch != c.Epoch {
			return ErrChannelAdmissionConflict
		}
		if err := validatePreparedTargetLink(tx, c, row, req, snapshotRead); err != nil {
			return err
		}
		if row.State == "abandoned" {
			result = row
			return nil
		}
		if row.State != "prepared" {
			if err := migrationCurrentHead(tx, row, req); err != nil {
				return err
			}
		}
		if row.State == "committed" {
			if err := migrationCommittedMatches(tx, row, req, snapshotRead); err != nil {
				return err
			}
		} else if row.State == "prepared" || row.State == "sealed" {
			if row.State == "prepared" {
				family, err := migrationFamilyHash(tx, req.Seal.From.KeyDigest, req.Seal.To.KeyDigest)
				if err != nil {
					return err
				}
				if family != req.FamilyHash {
					return ErrChannelAdmissionConflict
				}
			}
			if err := migrationSnapshotMatches(tx, req, migrationTargetStatus(req), snapshotRead); err != nil {
				return err
			}
		} else {
			return ErrChannelAdmissionConflict
		}
		var probe ChannelMigrationProbeRecord
		q := tx
		if !snapshotRead {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := q.First(&probe, "evidence_hash = ?", row.ProbeHash).Error; err != nil {
			return err
		}
		if probe.Revoked || probe.FactHash != row.ProbeFactHash {
			return ErrChannelAdmissionUnavailable
		}
		result = row
		return nil
	}()
	return
}

// Fetch DB storage values without invoking ModelConfig's map-any serializer.
// Every normal writer shares the control lock, including INSERT and DELETE;
// row locks alone would not protect a newly inserted member of the pin set.
func migrationConfigRows(tx *gorm.DB) ([]map[string]any, error) {
	return migrationRawRows(tx.Table("model_configs").Order("model"))
}
func migrationRawRows(query *gorm.DB) ([]map[string]any, error) {
	rows, err := query.Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := map[string]any{}
		for i, k := range cols {
			v := values[i]
			switch x := v.(type) {
			case []byte:
				v = string(x)
			case time.Time:
				v = x.UTC().Format(time.RFC3339Nano)
			}
			m[k] = v
		}
		result = append(result, m)
	}
	return result, rows.Err()
}
func migrationProjection(tx *gorm.DB, id int) (string, error) {
	rows, err := migrationRawRows(tx.Table("channels").Select("models, model_mapping, sets, configs, priority, backup_only").Where("id = ? AND deleted_at IS NULL", id))
	if err != nil {
		return "", err
	}
	if len(rows) != 1 {
		return "", ErrChannelAdmissionConflict
	}
	return admissionHash(rows[0])
}

type migrationConfigRewrite struct {
	model  string
	before string
	after  string
}

func migrationPlanConfigRewrites(rows []map[string]any, input ChannelMigrationRequest) ([]migrationConfigRewrite, error) {
	plan := []migrationConfigRewrite{}
	for _, row := range rows {
		name, ok := row["model"].(string)
		if !ok {
			return nil, ErrChannelAdmissionConflict
		}
		raw, ok := row["config"].(string)
		if !ok {
			if row["config"] == nil {
				continue
			}
			return nil, ErrChannelAdmissionConflict
		}
		next, changed, err := migrationRewriteConfig(raw, input)
		if err != nil {
			return nil, err
		}
		if changed {
			plan = append(plan, migrationConfigRewrite{name, raw, next})
		}
	}
	return plan, nil
}

// RawMessage retains unknown JSON numbers (including >2^53) and fields. Only
// three allowed identity fields change; prices and every other DB column stay
// byte-identical. No ModelConfig BeforeSave normalizations run on this path.
func migrationRewriteConfig(raw string, input ChannelMigrationRequest) (string, bool, error) {
	if raw == "" {
		return raw, false, nil
	}
	if err := migrationUniqueJSON([]byte(raw)); err != nil {
		return "", false, err
	}
	var cfg map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &cfg) != nil {
		return "", false, ErrChannelAdmissionConflict
	}
	quoteRaw, hasQuote := cfg["upstream_prepayment_v1"]
	bindingRaw, hasBinding := cfg["x_token_platform_native_result_v1"]
	if !hasQuote && !hasBinding {
		return raw, false, nil
	}
	if !hasQuote {
		return "", false, ErrChannelAdmissionConflict
	}
	var quote map[string]json.RawMessage
	if json.Unmarshal(quoteRaw, &quote) != nil || string(quote["version"]) != "1" {
		return "", false, ErrChannelAdmissionConflict
	}
	var routes []map[string]json.RawMessage
	if json.Unmarshal(quote["routes"], &routes) != nil || len(routes) == 0 {
		return "", false, ErrChannelAdmissionConflict
	}
	var binding map[string]json.RawMessage
	if hasBinding && (json.Unmarshal(bindingRaw, &binding) != nil || string(binding["version"]) != "1") {
		return "", false, ErrChannelAdmissionConflict
	}
	changed := false
	seen := map[int]bool{}
	routeIDs := map[string]bool{}
	bindingMatches := 0
	for _, route := range routes {
		var channel int
		var scope, routeID, endpoint string
		if json.Unmarshal(route["channelId"], &channel) != nil || channel <= 0 || json.Unmarshal(route["credentialScope"], &scope) != nil || scope == "" || json.Unmarshal(route["routeId"], &routeID) != nil || routeID == "" || json.Unmarshal(route["endpoint"], &endpoint) != nil || endpoint == "" || route["channelRef"] != nil || routeIDs[routeID] {
			return "", false, ErrChannelAdmissionConflict
		}
		routeIDs[routeID] = true
		var bc int
		var be string
		matches := hasBinding && json.Unmarshal(binding["channelId"], &bc) == nil && json.Unmarshal(binding["endpoint"], &be) == nil && bc == channel && be == endpoint
		if matches {
			bindingMatches++
		}
		if channel == input.Seal.From.ChannelID {
			if scope != input.FromCredentialID {
				return "", false, ErrChannelAdmissionConflict
			}
			if fp, ok := route["keyFingerprint"]; ok {
				var digest string
				if json.Unmarshal(fp, &digest) != nil || digest != input.Seal.From.KeyDigest {
					return "", false, ErrChannelAdmissionConflict
				}
				route["keyFingerprint"], _ = json.Marshal(input.Seal.To.KeyDigest)
			}
			channel = input.Seal.To.ChannelID
			route["channelId"], _ = json.Marshal(channel)
			route["credentialScope"], _ = json.Marshal(input.ToCredentialID)
			changed = true
			if matches {
				var bscope, digest string
				if binding["channelRef"] != nil || json.Unmarshal(binding["credentialScope"], &bscope) != nil || bscope != input.FromCredentialID || json.Unmarshal(binding["keyFingerprint"], &digest) != nil || digest != input.Seal.From.KeyDigest {
					return "", false, ErrChannelAdmissionConflict
				}
				binding["channelId"], _ = json.Marshal(channel)
				binding["credentialScope"], _ = json.Marshal(input.ToCredentialID)
				binding["keyFingerprint"], _ = json.Marshal(input.Seal.To.KeyDigest)
			}
		}
		if seen[channel] {
			return "", false, ErrChannelAdmissionConflict
		}
		seen[channel] = true
	}
	if hasBinding && bindingMatches != 1 {
		return "", false, ErrChannelAdmissionConflict
	}
	if !changed {
		return raw, false, nil
	}
	quote["routes"], _ = json.Marshal(routes)
	cfg["upstream_prepayment_v1"], _ = json.Marshal(quote)
	if hasBinding {
		cfg["x_token_platform_native_result_v1"], _ = json.Marshal(binding)
	}
	result, err := json.Marshal(cfg)
	if err != nil {
		return "", false, fmt.Errorf("migration JSON encoding: %w", err)
	}
	return string(result), true, nil
}

func migrationFamilyHash(tx *gorm.DB, from, to string) (string, error) {
	f, err := admissionFamily(tx, from)
	if err != nil {
		return "", err
	}
	t, err := admissionFamily(tx, to)
	if err != nil {
		return "", err
	}
	return admissionHash([]any{f, t})
}

// Duplicate keys are ambiguous to different JSON consumers; reject rather than
// choosing a last value and accidentally authorizing a different frozen fact.
func migrationUniqueJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func() error
	value = func() error {
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return ErrChannelAdmissionConflict
				}
				seen[name] = true
				if err := value(); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return ErrChannelAdmissionConflict
			}
		case '[':
			for d.More() {
				if err := value(); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return ErrChannelAdmissionConflict
			}
		default:
			return ErrChannelAdmissionConflict
		}
		return nil
	}
	if err := value(); err != nil {
		return ErrChannelAdmissionConflict
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrChannelAdmissionConflict
	}
	return nil
}

// AbandonPrepared releases only an unsealed preparation's editing restriction.
// It retains the shadow, probe/use evidence and audit row. It never disables a
// live channel, refunds work, closes a use or grants key-revocation permission.
// No verifier is needed to relinquish an unconsumed intent; exact persisted
// identities, request hash, storage generation and absence of grants are checked.
func (s *ChannelAdmissionStore) AbandonPrepared(ctx context.Context, operationID, requestHash string) (result *ChannelMigrationPrepared, err error) {
	if !fullIdentityDigest(requestHash) {
		return nil, ErrChannelAdmissionConflict
	}
	err = s.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		if _, err := loadShadowResource(tx, operationID, 0, false); err != nil {
			return err
		}
		row, err := loadMigration(tx, operationID)
		if err != nil {
			return err
		}
		if row.RequestHash != requestHash {
			return ErrChannelAdmissionConflict
		}
		var original ChannelMigrationRequest
		if json.Unmarshal([]byte(row.RequestJSON), &original) != nil {
			return ErrChannelAdmissionConflict
		}
		if err := validatePreparedShadowLink(tx, c, row, original, false); err != nil {
			return err
		}
		if row.ShadowCheckpointHash != "" {
			resource, e := loadShadowResource(tx, operationID, row.ToChannelID, false)
			if e != nil || resource == nil {
				return ErrChannelAdmissionConflict
			}
			if _, e := cancelShadowTx(tx, c, resource, resource.RequestHash, resource.CheckpointHash); e != nil {
				return e
			}
			row.State = "abandoned"
		} else if e := abandonPreparedTx(tx, c, row, requestHash); e != nil {
			return e
		}

		result = row
		return nil
	})
	return
}

func abandonPreparedTx(tx *gorm.DB, c *ChannelAdmissionControl, row *ChannelMigrationPrepared, requestHash string) error {
	operationID := row.OperationID
	var req ChannelMigrationRequest
	if json.Unmarshal([]byte(row.RequestJSON), &req) != nil {
		return ErrChannelAdmissionConflict
	}
	hash, _ := admissionHash(req)
	if row.RequestHash != requestHash || hash != requestHash || req.Seal.OperationID != operationID || row.StorageGeneration != c.StorageGeneration || row.FromChannelID != req.Seal.From.ChannelID || row.ToChannelID != req.Seal.To.ChannelID || row.SealRevision != 0 || row.CommitRevision != 0 || row.CommittedAtMs != 0 || row.AfterPinHash != "" || (row.State != "prepared" && row.State != "abandoned") {
		return ErrChannelAdmissionConflict
	}
	if _, err := storedAdmissionIdentity(tx, req.Seal.From); err != nil {
		return err
	}
	shadow, err := storedAdmissionIdentity(tx, req.Seal.To)
	if err != nil {
		return err
	}
	if shadow.Status != ChannelStatusDisabled {
		return ErrChannelAdmissionConflict
	}
	for _, table := range []any{&ChannelAdmissionBarrier{}, &ChannelAdmissionFamily{}} {
		var count int64
		if err := tx.Model(table).Where("operation_id = ?", operationID).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return ErrChannelAdmissionConflict
		}
	}
	if row.State == "prepared" {
		update := tx.Model(&ChannelMigrationPrepared{}).Where("operation_id = ? AND state = ? AND request_hash = ?", operationID, "prepared", requestHash).UpdateColumn("state", "abandoned")
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrChannelAdmissionConflict
		}
		row.State = "abandoned"
	}
	return nil
}

// commitPreparedPinsTx requires the same held control transaction as sealTx.
func (s *ChannelAdmissionStore) commitPreparedPinsTx(tx *gorm.DB, c *ChannelAdmissionControl, a *migrationAuthorization, sealRevision int64) (result *ChannelMigrationPrepared, err error) {
	input := a.request
	err = func() error {
		if err := validateMigrationAuthorization(tx, c, a, "commit", s.MigrationFeatures); err != nil {
			return err
		}
		row, err := loadMigration(tx, input.Seal.OperationID)
		if err != nil {
			return err
		}
		if row.SealRevision != sealRevision {
			return ErrChannelAdmissionConflict
		}
		if row.State == "committed" {
			if err := migrationCommittedMatches(tx, row, input); err != nil {
				return err
			}
			if _, err := lockDeploymentProof(tx, c, a.deployment, s.Executor); err != nil {
				return err
			}
			result = row
			return finalizeMigrationProbe(tx, a.probe)
		}
		if err := migrationSnapshotMatches(tx, input, migrationTargetStatus(input)); err != nil {
			return err
		}
		if _, err := lockDeploymentProof(tx, c, a.deployment, s.Executor); err != nil {
			return err
		}
		configs, err := migrationConfigRows(tx)
		if err != nil {
			return err
		}
		plan, err := migrationPlanConfigRewrites(configs, input)
		if err != nil {
			return err
		}
		for _, change := range plan {
			update := tx.Table("model_configs").Where("model = ? AND config = ?", change.model, change.before).UpdateColumn("config", change.after)
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected != 1 {
				return ErrChannelAdmissionConflict
			}
		}
		if !reverseMigration(input) {
			update := tx.Model(&Channel{}).Where("id = ? AND status = ?", input.Seal.To.ChannelID, ChannelStatusDisabled).UpdateColumn("status", ChannelStatusEnabled)
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected != 1 {
				return ErrChannelAdmissionConflict
			}
		}

		after, err := migrationConfigRows(tx)
		if err != nil {
			return err
		}
		hash, err := admissionHash(after)
		if err != nil {
			return err
		}
		now, err := channelAdmissionTime(tx)
		if err != nil {
			return err
		}
		row.State = "committed"
		row.AfterPinHash = hash
		row.CommitRevision = c.Revision + 1
		row.CommittedAtMs = now.UnixMilli()
		save := tx
		if row.ShadowCheckpointHash == "" {
			save = save.Omit("ShadowCheckpointHash")
		}
		if err := save.Save(row).Error; err != nil {
			return err
		}
		if err := finalizeMigrationProbe(tx, a.probe); err != nil {
			return err
		}
		if err := finalizeAdmissionProof(tx, c, a.deployment, row.CommitRevision); err != nil {
			return err
		}
		if _, err := lockDeploymentProof(tx, c, a.deployment, s.Executor); err != nil {
			return err
		}
		if err := finalizeMigrationProbe(tx, a.probe); err != nil {
			return err
		}
		result = row
		return nil
	}()
	return
}
