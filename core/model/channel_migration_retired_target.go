package model

// Optional, server-only reuse of an enabled retired physical identity. No channel
// creation, status mutation, key revocation, task rewrite or use closure occurs.
import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const MigrationReuseRetiredTarget = "reuse_retired_target"

type MigrationRetiredTargetRequest struct {
	Version            int                     `json:"version"`
	Command            ChannelMigrationRequest `json:"command"`
	SourceSnapshotHash string                  `json:"sourceSnapshotHash"`
}

type ChannelMigrationRetiredTargetResource struct {
	OperationID           string `gorm:"primaryKey;size:128"`
	Version               int    `gorm:"not null"`
	RequestJSON           string `gorm:"type:text;not null"`
	RequestHash           string `gorm:"size:64;not null"`
	CheckpointHash        string `gorm:"size:64;not null"`
	Epoch                 string `gorm:"size:128;not null"`
	StorageGeneration     string `gorm:"size:128;not null"`
	FromChannelID         int    `gorm:"not null;index"`
	TargetChannelID       int    `gorm:"not null;index"`
	SourceRevision        int64  `gorm:"not null"`
	CreatedRevision       int64  `gorm:"not null"`
	CreatedAtMs           int64  `gorm:"not null"`
	RetirementOperationID string `gorm:"size:128;not null"`
	RetirementRevision    int64  `gorm:"not null"`
	RetirementRequestHash string `gorm:"size:64;not null"`
	LifecycleState        string `gorm:"size:16;not null"`
	LifecycleRevision     int64  `gorm:"not null"`
	PreparedRequestHash   string `gorm:"size:64;not null;default:''"`
	PreparedProbeHash     string `gorm:"size:64;not null;default:''"`
	PreparedProbeFactHash string `gorm:"size:64;not null;default:''"`
	PreparedAtMs          int64  `gorm:"not null;default:0"`
	CancelledAtMs         int64  `gorm:"not null;default:0"`
}

func reverseMigration(r ChannelMigrationRequest) bool {
	return r.TargetResourceKind == MigrationReuseRetiredTarget && ValidMigrationReadOperationID(r.Seal.RevertOf)
}
func validMigrationResourceKind(r ChannelMigrationRequest) bool {
	return (r.TargetResourceKind == "" && r.Seal.RevertOf == "") || reverseMigration(r)
}
func migrationTargetStatus(r ChannelMigrationRequest) int {
	if reverseMigration(r) {
		return ChannelStatusEnabled
	}
	return ChannelStatusDisabled
}
func retiredStoragePresent(tx *gorm.DB) (bool, error) {
	var n int64
	var e error
	switch tx.Dialector.Name() {
	case "sqlite":
		e = tx.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", "channel_migration_retired_target_resources").Scan(&n).Error
	case "postgres":
		e = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=?", "channel_migration_retired_target_resources").Scan(&n).Error
	case "mysql":
		e = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?", "channel_migration_retired_target_resources").Scan(&n).Error
	default:
		return false, ErrChannelAdmissionUnavailable
	}
	return n > 0, e
}
func retiredStorageReady(tx *gorm.DB) error {
	st := &gorm.Statement{DB: tx}
	if st.Parse(&ChannelMigrationRetiredTargetResource{}) != nil {
		return ErrChannelAdmissionUnavailable
	}
	cols := make([]clause.Column, 0, len(st.Schema.DBNames))
	for _, name := range st.Schema.DBNames {
		cols = append(cols, clause.Column{Table: st.Schema.Table, Name: name})
	}
	rows, e := tx.Model(&ChannelMigrationRetiredTargetResource{}).Clauses(clause.Select{Columns: cols}).Limit(0).Rows()
	if e != nil {
		return ErrChannelAdmissionUnavailable
	}
	e = rows.Err()
	ce := rows.Close()
	if e != nil || ce != nil {
		return ErrChannelAdmissionUnavailable
	}
	var bad int64
	if tx.Model(&ChannelMigrationRetiredTargetResource{}).Where("version IS NULL OR version <> 1 OR lifecycle_state IS NULL OR lifecycle_state NOT IN ?", []string{"created", "consumed", "cancelled"}).Count(&bad).Error != nil || bad != 0 {
		return ErrChannelAdmissionUnavailable
	}
	return nil
}
func retiredCheckpoint(r *ChannelMigrationRetiredTargetResource) string {
	h, _ := admissionHash([]any{"retired-target-v1", r.OperationID, r.RequestHash, r.Epoch, r.StorageGeneration, r.FromChannelID, r.TargetChannelID, r.SourceRevision, r.CreatedRevision, r.CreatedAtMs, r.RetirementOperationID, r.RetirementRevision, r.RetirementRequestHash})
	return h
}
func retiredRequest(r *ChannelMigrationRetiredTargetResource) (q MigrationRetiredTargetRequest, e error) {
	if json.Unmarshal([]byte(r.RequestJSON), &q) != nil {
		return q, ErrChannelAdmissionConflict
	}
	hash, _ := admissionHash(q)
	if r.Version != 1 || q.Version != 1 || !validProbeCommand(q.Command) || !reverseMigration(q.Command) || !ValidMigrationReadOperationID(q.Command.Seal.OperationID) || !fullIdentityDigest(q.SourceSnapshotHash) || hash != r.RequestHash || r.CheckpointHash != retiredCheckpoint(r) || r.OperationID != q.Command.Seal.OperationID || r.FromChannelID != q.Command.Seal.From.ChannelID || r.TargetChannelID != q.Command.Seal.To.ChannelID || r.Epoch != q.Command.Seal.Epoch || r.SourceRevision != q.Command.Seal.ExpectedRevision || r.CreatedRevision <= r.SourceRevision || r.CreatedAtMs <= 0 || r.StorageGeneration == "" || r.RetirementOperationID != q.Command.Seal.RevertOf || r.RetirementRevision <= 0 || r.RetirementRevision > r.SourceRevision || !fullIdentityDigest(r.RetirementRequestHash) || r.LifecycleRevision < r.CreatedRevision {
		return q, ErrChannelAdmissionConflict
	}
	bound := fullIdentityDigest(r.PreparedRequestHash) && fullIdentityDigest(r.PreparedProbeHash) && fullIdentityDigest(r.PreparedProbeFactHash) && r.PreparedAtMs >= r.CreatedAtMs
	empty := r.PreparedRequestHash == "" && r.PreparedProbeHash == "" && r.PreparedProbeFactHash == "" && r.PreparedAtMs == 0
	switch r.LifecycleState {
	case "created":
		if !empty || r.CancelledAtMs != 0 || r.LifecycleRevision != r.CreatedRevision {
			return q, ErrChannelAdmissionConflict
		}
	case "consumed":
		if !bound || r.CancelledAtMs != 0 || r.LifecycleRevision <= r.CreatedRevision {
			return q, ErrChannelAdmissionConflict
		}
	case "cancelled":
		if (!bound && !empty) || r.CancelledAtMs < r.CreatedAtMs || r.CancelledAtMs < r.PreparedAtMs || r.LifecycleRevision <= r.CreatedRevision {
			return q, ErrChannelAdmissionConflict
		}
	default:
		return q, ErrChannelAdmissionConflict
	}
	return
}
func loadRetiredResource(tx *gorm.DB, op string, snapshot bool) (*ChannelMigrationRetiredTargetResource, error) {
	if e := retiredStorageReady(tx); e != nil {
		return nil, e
	}
	q := tx
	if !snapshot {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var r ChannelMigrationRetiredTargetResource
	if e := q.First(&r, "operation_id=?", op).Error; e != nil {
		return nil, e
	}
	if _, e := retiredRequest(&r); e != nil {
		return nil, e
	}
	return &r, nil
}

// Historical evidence is deliberately distinct from migrationCurrentHead. A
// previous committed forward can be historical after a legal stack unwind.
func retiredHistory(tx *gorm.DB, c *ChannelAdmissionControl, req ChannelMigrationRequest) (*ChannelAdmissionBarrier, error) {
	if !reverseMigration(req) || req.Seal.Epoch != c.Epoch {
		return nil, ErrChannelAdmissionConflict
	}
	var b ChannelAdmissionBarrier
	if e := tx.Where("from_channel_id=?", req.Seal.To.ChannelID).Order("revision DESC").First(&b).Error; e != nil {
		return nil, e
	}
	if b.OperationID != req.Seal.RevertOf || b.Version != ChannelAdmissionProtocolVersion || b.Epoch != c.Epoch || b.Revision <= 0 || b.Revision > c.Revision || b.RecordedAtMs <= 0 || b.FromChannelID != req.Seal.To.ChannelID || b.FromKeyDigest != req.Seal.To.KeyDigest || b.FromIdentityHash != req.Seal.To.IdentityHash || b.ToChannelID != req.Seal.From.ChannelID || b.ToKeyDigest != req.Seal.From.KeyDigest || b.ToIdentityHash != req.Seal.From.IdentityHash {
		return nil, ErrChannelAdmissionConflict
	}
	old, e := loadMigration(tx, b.OperationID, true)
	if e != nil {
		return nil, e
	}
	var original ChannelMigrationRequest
	if json.Unmarshal([]byte(old.RequestJSON), &original) != nil {
		return nil, ErrChannelAdmissionConflict
	}
	hash, _ := admissionHash(original.Seal)
	if old.State != "committed" || original.Seal.Epoch != b.Epoch || old.SealRevision != b.Revision || old.CommitRevision > c.Revision || old.StorageGeneration != c.StorageGeneration || hash != b.RequestHash || original.Seal.From != req.Seal.To || original.Seal.To != req.Seal.From || original.FromCredentialID != req.ToCredentialID || original.ToCredentialID != req.FromCredentialID {
		return nil, ErrChannelAdmissionConflict
	}
	// Immutable reciprocal resource evidence, not its obsolete family/pin snapshot.
	if e = validatePreparedTargetLink(tx, c, old, original, true); e != nil {
		return nil, e
	}
	return &b, nil
}
func retiredCurrent(tx *gorm.DB, c *ChannelAdmissionControl, r *ChannelMigrationRetiredTargetResource, req ChannelMigrationRequest, features []string) error {
	original, e := retiredRequest(r)
	if e != nil {
		return e
	}
	hash, _ := admissionHash(req)
	want, _ := admissionHash(original.Command)
	if hash != want || r.Epoch != c.Epoch || r.StorageGeneration != c.StorageGeneration || r.LifecycleRevision > c.Revision || r.LifecycleState == "cancelled" {
		return ErrChannelAdmissionConflict
	}
	b, e := retiredHistory(tx, c, req)
	if e != nil {
		return e
	}
	if b.Revision != r.RetirementRevision || b.RequestHash != r.RetirementRequestHash {
		return ErrChannelAdmissionConflict
	}
	family, e := admissionFamily(tx, req.Seal.From.KeyDigest)
	if e != nil {
		return e
	}
	if family == nil || !familyAllows(family, req.Seal.From) {
		return ErrChannelAdmissionConflict
	}
	fh, e := migrationFamilyHash(tx, req.Seal.From.KeyDigest, req.Seal.To.KeyDigest)
	if e != nil || fh != req.FamilyHash {
		return ErrChannelAdmissionConflict
	}
	if e = migrationSnapshotMatches(tx, req, ChannelStatusEnabled); e != nil {
		return e
	}
	inv, e := migrationInventoryTx(tx, c, features)
	if e != nil || inv.State != "ready" || !inv.Complete {
		return ErrChannelAdmissionUnavailable
	}
	inv.Revision = strconv.FormatInt(r.SourceRevision, 10)
	inv.SourceSnapshotHash = ""
	ih, _ := admissionHash(inv)
	if ih != original.SourceSnapshotHash {
		return ErrChannelAdmissionConflict
	}
	return nil
}
func (s *ChannelAdmissionStore) PrepareRetiredTargetResource(ctx context.Context, q MigrationRetiredTargetRequest) (out *ChannelMigrationRetiredTargetResource, err error) {
	if q.Version != 1 || !validProbeCommand(q.Command) || !reverseMigration(q.Command) || !ValidMigrationReadOperationID(q.Command.Seal.OperationID) || !fullIdentityDigest(q.SourceSnapshotHash) {
		return nil, ErrChannelAdmissionConflict
	}
	proof, e := s.deployment(ctx)
	if e != nil {
		return nil, e
	}
	err = s.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		if e := retiredStorageReady(tx); e != nil {
			return e
		}
		hash, _ := admissionHash(q)
		r, e := loadRetiredResource(tx, q.Command.Seal.OperationID, false)
		if e == nil {
			if r.RequestHash != hash || r.Epoch != c.Epoch || r.StorageGeneration != c.StorageGeneration || r.LifecycleRevision > c.Revision {
				return ErrChannelAdmissionConflict
			}
			out = r
			return nil
		}
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		if c.Revision != q.Command.Seal.ExpectedRevision || q.Command.Seal.Epoch != c.Epoch {
			return ErrChannelAdmissionConflict
		}
		if e = shadowNoOperationEffects(tx, q.Command.Seal.OperationID); e != nil {
			return e
		}
		if present, e := shadowStoragePresent(tx); e != nil {
			return e
		} else if present {
			var n int64
			if e = tx.Model(&ChannelMigrationShadowResource{}).Where("operation_id=?", q.Command.Seal.OperationID).Count(&n).Error; e != nil {
				return e
			}
			if n != 0 {
				return ErrChannelAdmissionConflict
			}
		}
		// Pending owners conflict; historical committed rows on the same ID coexist.
		ids := []int{q.Command.Seal.From.ChannelID, q.Command.Seal.To.ChannelID}
		var n int64
		if e = tx.Model(&ChannelMigrationPrepared{}).Where("state IN ? AND (from_channel_id IN ? OR to_channel_id IN ?)", []string{"prepared", "sealed"}, ids, ids).Count(&n).Error; e != nil {
			return e
		}
		if n != 0 {
			return ErrChannelAdmissionConflict
		}
		if e = tx.Model(&ChannelMigrationRetiredTargetResource{}).Where("lifecycle_state='created' AND (from_channel_id IN ? OR target_channel_id IN ?)", ids, ids).Count(&n).Error; e != nil {
			return e
		}
		if n != 0 {
			return ErrChannelAdmissionConflict
		}
		b, e := retiredHistory(tx, c, q.Command)
		if e != nil {
			return e
		}
		if e = lockShadowSourceRows(tx); e != nil {
			return e
		}
		now, e := lockDeploymentProof(tx, c, proof, s.Executor)
		if e != nil {
			return e
		}
		raw, _ := json.Marshal(q)
		r = &ChannelMigrationRetiredTargetResource{OperationID: q.Command.Seal.OperationID, Version: 1, RequestJSON: string(raw), RequestHash: hash, Epoch: c.Epoch, StorageGeneration: c.StorageGeneration, FromChannelID: ids[0], TargetChannelID: ids[1], SourceRevision: c.Revision, CreatedRevision: c.Revision + 1, CreatedAtMs: now.UnixMilli(), RetirementOperationID: b.OperationID, RetirementRevision: b.Revision, RetirementRequestHash: b.RequestHash, LifecycleState: "created", LifecycleRevision: c.Revision + 1}
		r.CheckpointHash = retiredCheckpoint(r)
		if e = tx.Create(r).Error; e != nil {
			return e
		}
		if e = finalizeAdmissionProof(tx, c, proof, c.Revision+1); e != nil {
			return e
		}
		c.Revision++
		if e = retiredCurrent(tx, c, r, q.Command, s.MigrationFeatures); e != nil {
			return e
		}
		if _, e = lockDeploymentProof(tx, c, proof, s.Executor); e != nil {
			return e
		}
		out = r
		return nil
	})
	return
}
func validateRetiredPreparedLink(tx *gorm.DB, c *ChannelAdmissionControl, p *ChannelMigrationPrepared, req ChannelMigrationRequest, snapshot bool) error {
	r, e := loadRetiredResource(tx, p.OperationID, snapshot)
	if e != nil {
		return e
	}
	q, e := retiredRequest(r)
	if e != nil {
		return e
	}
	h, _ := admissionHash(q.Command)
	rh, _ := admissionHash(req)
	if h != rh || r.Epoch != c.Epoch || r.StorageGeneration != c.StorageGeneration || r.LifecycleRevision > c.Revision || p.ShadowCheckpointHash != r.CheckpointHash || p.RequestHash != r.PreparedRequestHash || p.ProbeHash != r.PreparedProbeHash || p.ProbeFactHash != r.PreparedProbeFactHash || p.FromChannelID != r.FromChannelID || p.ToChannelID != r.TargetChannelID || p.StorageGeneration != r.StorageGeneration {
		return ErrChannelAdmissionConflict
	}
	if p.State == "abandoned" {
		if r.LifecycleState != "cancelled" {
			return ErrChannelAdmissionConflict
		}
	} else if r.LifecycleState != "consumed" {
		return ErrChannelAdmissionConflict
	}
	return nil
}
func validatePreparedTargetLink(tx *gorm.DB, c *ChannelAdmissionControl, p *ChannelMigrationPrepared, req ChannelMigrationRequest, snapshot bool) error {
	if !validMigrationResourceKind(req) {
		return ErrChannelAdmissionConflict
	}
	if reverseMigration(req) {
		return validateRetiredPreparedLink(tx, c, p, req, snapshot)
	}
	return validatePreparedShadowLink(tx, c, p, req, snapshot)
}
func lockMigrationTarget(tx *gorm.DB, req ChannelMigrationRequest) error {
	if !validMigrationResourceKind(req) {
		return ErrChannelAdmissionConflict
	}
	if reverseMigration(req) {
		_, e := loadRetiredResource(tx, req.Seal.OperationID, false)
		return e
	}
	_, e := loadShadowResource(tx, req.Seal.OperationID, req.Seal.To.ChannelID, false)
	return e
}

// Protect the resource-to-prepare gap without altering legacy forward semantics.
func guardRetiredTargetMutation(tx *gorm.DB, ids []int) error {
	present, e := retiredStoragePresent(tx)
	if e != nil || !present {
		return e
	}
	if e = retiredStorageReady(tx); e != nil {
		return e
	}
	var n int64
	if e = tx.Model(&ChannelMigrationRetiredTargetResource{}).Where("lifecycle_state='created' AND (from_channel_id IN ? OR target_channel_id IN ?)", ids, ids).Count(&n).Error; e != nil {
		return e
	}
	if n != 0 {
		return ErrChannelAdmissionConflict
	}
	return nil
}

func rejectRetiredOperationCollision(tx *gorm.DB, op string) error {
	present, e := retiredStoragePresent(tx)
	if e != nil || !present {
		return e
	}
	if e = retiredStorageReady(tx); e != nil {
		return e
	}
	var n int64
	if e = tx.Model(&ChannelMigrationRetiredTargetResource{}).Where("operation_id=?", op).Count(&n).Error; e != nil {
		return e
	}
	if n != 0 {
		return ErrChannelAdmissionConflict
	}
	return nil
}
func addPendingRetiredIDs(tx *gorm.DB, ids map[int]bool) error {
	present, e := retiredStoragePresent(tx)
	if e != nil || !present {
		return e
	}
	if e = retiredStorageReady(tx); e != nil {
		return e
	}
	var rows []ChannelMigrationRetiredTargetResource
	if e = tx.Where("lifecycle_state='created'").Find(&rows).Error; e != nil {
		return e
	}
	for _, r := range rows {
		if _, e = retiredRequest(&r); e != nil {
			return e
		}
		ids[r.FromChannelID] = true
		ids[r.TargetChannelID] = true
	}
	return nil
}
