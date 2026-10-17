package model

import (
	"context"
	"encoding/json"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// This server-only lifecycle never deletes a resource, revokes a key, closes a
// use, refunds work, or proves an executor has stopped. A cancelled shadow is
// retained and remains protected from ordinary management writes.
type MigrationShadowLifecycleReceipt struct {
	OperationID           string
	CheckpointHash        string
	State                 string
	Revision              int64
	PreparedRequestHash   string
	PreparedProbeHash     string
	PreparedProbeFactHash string
	PreparedAtMs          int64
	CancelledAtMs         int64
}

func shadowLifecycleReceipt(r *ChannelMigrationShadowResource) *MigrationShadowLifecycleReceipt {
	return &MigrationShadowLifecycleReceipt{r.OperationID, r.CheckpointHash, r.LifecycleState, r.LifecycleRevision, r.PreparedRequestHash, r.PreparedProbeHash, r.PreparedProbeFactHash, r.PreparedAtMs, r.CancelledAtMs}
}
func shadowLifecycleValid(r *ChannelMigrationShadowResource) error {
	bound := fullIdentityDigest(r.PreparedRequestHash) && fullIdentityDigest(r.PreparedProbeHash) && fullIdentityDigest(r.PreparedProbeFactHash) && r.PreparedAtMs >= r.CreatedAtMs
	empty := r.PreparedRequestHash == "" && r.PreparedProbeHash == "" && r.PreparedProbeFactHash == "" && r.PreparedAtMs == 0
	if r.LifecycleRevision < r.CreatedRevision {
		return ErrChannelAdmissionConflict
	}
	switch r.LifecycleState {
	case "created":
		if !empty || r.CancelledAtMs != 0 || r.LifecycleRevision != r.CreatedRevision {
			return ErrChannelAdmissionConflict
		}
	case "consumed":
		if !bound || r.CancelledAtMs != 0 || r.LifecycleRevision <= r.CreatedRevision {
			return ErrChannelAdmissionConflict
		}
	case "cancelled":
		if (!bound && !empty) || r.CancelledAtMs < r.CreatedAtMs || r.CancelledAtMs < r.PreparedAtMs || r.LifecycleRevision <= r.CreatedRevision {
			return ErrChannelAdmissionConflict
		}
	default:
		return ErrChannelAdmissionUnavailable // old unknown is never backfilled
	}
	return nil
}
func loadShadowResource(tx *gorm.DB, op string, channelID int, snapshot bool) (*ChannelMigrationShadowResource, error) {
	present, e := shadowStoragePresent(tx)
	if e != nil || !present {
		return nil, e
	}
	if e = shadowStorageReady(tx); e != nil {
		return nil, e
	}
	q := tx.Where("operation_id = ? OR shadow_channel_id = ?", op, channelID)
	if !snapshot {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var rows []ChannelMigrationShadowResource
	if e = q.Find(&rows).Error; e != nil {
		return nil, e
	}
	if len(rows) == 0 {
		return nil, nil
	}
	if len(rows) != 1 || rows[0].OperationID != op || (channelID > 0 && rows[0].ShadowChannelID != channelID) {
		return nil, ErrChannelAdmissionConflict
	}
	r := &rows[0]
	if _, e = shadowResourceRequest(r); e != nil {
		return nil, e
	}
	if e = shadowLifecycleValid(r); e != nil {
		return nil, e
	}
	return r, nil
}
func shadowContextMatches(c *ChannelAdmissionControl, r *ChannelMigrationShadowResource) bool {
	return c.Epoch == r.SourceEpoch && c.StorageGeneration == r.StorageGeneration && c.Revision >= r.LifecycleRevision
}
func shadowPreparedMatches(r *ChannelMigrationShadowResource, p *ChannelMigrationPrepared) bool {
	return p.OperationID == r.OperationID && p.ShadowCheckpointHash == r.CheckpointHash && p.RequestHash == r.PreparedRequestHash && p.ProbeHash == r.PreparedProbeHash && p.ProbeFactHash == r.PreparedProbeFactHash && p.StorageGeneration == r.StorageGeneration && p.FromChannelID == r.FromChannelID && p.ToChannelID == r.ShadowChannelID
}
func validatePreparedShadowLink(tx *gorm.DB, c *ChannelAdmissionControl, p *ChannelMigrationPrepared, req ChannelMigrationRequest, snapshot bool) error {
	r, e := loadShadowResource(tx, p.OperationID, p.ToChannelID, snapshot)
	if e != nil {
		return e
	}
	if r == nil {
		if p.ShadowCheckpointHash != "" {
			return ErrChannelAdmissionConflict
		}
		return nil
	}
	original, e := shadowResourceRequest(r)
	if e != nil {
		return e
	}
	if !shadowContextMatches(c, r) || !shadowPreparedMatches(r, p) || req.Seal.From != original.Source.From || req.Seal.To != shadowIdentity(r) || req.ToCredentialID != original.Target.CredentialID {
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
func prepareShadowResource(tx *gorm.DB, c *ChannelAdmissionControl, req ChannelMigrationRequest, features []string) (*ChannelMigrationShadowResource, error) {
	r, e := loadShadowResource(tx, req.Seal.OperationID, req.Seal.To.ChannelID, false)
	if e != nil || r == nil {
		return r, e
	}
	original, e := shadowResourceRequest(r)
	if e != nil {
		return nil, e
	}
	if !shadowContextMatches(c, r) || req.Seal.From != original.Source.From || req.Seal.To != shadowIdentity(r) || req.ToCredentialID != original.Target.CredentialID || req.Seal.ExpectedRevision < r.CreatedRevision {
		return nil, ErrChannelAdmissionConflict
	}
	if r.LifecycleState == "consumed" {
		return r, nil
	} // existing Prepared is checked by its original exact authorization
	if r.LifecycleState != "created" {
		return nil, ErrChannelAdmissionConflict
	}
	if e = shadowNoOperationEffects(tx, r.OperationID); e != nil {
		return nil, e
	}
	if e = lockShadowSourceRows(tx); e != nil {
		return nil, e
	}
	if _, e = shadowObserve(tx, c, r, features); e != nil {
		return nil, e
	}
	return r, nil
}
func shadowAdvanceRevision(tx *gorm.DB, c *ChannelAdmissionControl) error {
	res := tx.Model(&ChannelAdmissionControl{}).Where("id=1 AND revision=?", c.Revision).UpdateColumn("revision", c.Revision+1)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return ErrChannelAdmissionConflict
	}
	c.Revision++
	return nil
}
func consumeShadowResource(tx *gorm.DB, c *ChannelAdmissionControl, r *ChannelMigrationShadowResource, p *ChannelMigrationPrepared) error {
	if r.LifecycleState != "created" || p.ShadowCheckpointHash != r.CheckpointHash {
		return ErrChannelAdmissionConflict
	}
	now, e := channelAdmissionTime(tx)
	if e != nil {
		return e
	}
	if now.UnixMilli() < r.CreatedAtMs {
		return ErrChannelAdmissionConflict
	}
	if e = shadowAdvanceRevision(tx, c); e != nil {
		return e
	}
	changes := map[string]any{"lifecycle_state": "consumed", "lifecycle_revision": c.Revision, "prepared_request_hash": p.RequestHash, "prepared_probe_hash": p.ProbeHash, "prepared_probe_fact_hash": p.ProbeFactHash, "prepared_at_ms": now.UnixMilli()}
	res := tx.Model(r).Where("lifecycle_state='created' AND checkpoint_hash=?", r.CheckpointHash).Updates(changes)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return ErrChannelAdmissionConflict
	}
	durable, e := loadMigration(tx, p.OperationID)
	if e != nil {
		return e
	}
	if *durable != *p {
		return ErrChannelAdmissionConflict
	}
	var req ChannelMigrationRequest
	if json.Unmarshal([]byte(p.RequestJSON), &req) != nil {
		return ErrChannelAdmissionConflict
	}
	return validatePreparedShadowLink(tx, c, p, req, false)
}
func shadowNoSealEffects(tx *gorm.DB, op string) error {
	for _, table := range []any{&ChannelAdmissionBarrier{}, &ChannelAdmissionFamily{}} {
		var n int64
		if e := tx.Model(table).Where("operation_id=?", op).Count(&n).Error; e != nil {
			return e
		}
		if n != 0 {
			return ErrChannelAdmissionConflict
		}
	}
	return nil
}
func cancelShadowTx(tx *gorm.DB, c *ChannelAdmissionControl, r *ChannelMigrationShadowResource, requestHash, checkpointHash string) (*MigrationShadowLifecycleReceipt, error) {
	if r.RequestHash != requestHash || r.CheckpointHash != checkpointHash || !shadowContextMatches(c, r) {
		return nil, ErrChannelAdmissionConflict
	}
	if _, e := shadowResourceRequest(r); e != nil {
		return nil, e
	}
	if e := shadowLifecycleValid(r); e != nil {
		return nil, e
	}
	var p ChannelMigrationPrepared
	e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&p, "operation_id=?", r.OperationID).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		if r.PreparedRequestHash != "" {
			return nil, ErrChannelAdmissionConflict
		}
	} else if e != nil {
		return nil, e
	} else {
		if !shadowPreparedMatches(r, &p) {
			return nil, ErrChannelAdmissionConflict
		}
		if (r.LifecycleState == "consumed" && p.State != "prepared") || (r.LifecycleState == "cancelled" && p.State != "abandoned") || r.LifecycleState == "created" {
			return nil, ErrChannelAdmissionConflict
		}
		if e = abandonPreparedTx(tx, c, &p, r.PreparedRequestHash); e != nil {
			return nil, e
		}
	}
	// Deliberately do not require the original full inventory/family to still
	// match. Cancellation stops future work and retains all existing evidence.
	target, targetErr := storedAdmissionIdentity(tx, shadowIdentity(r))
	if targetErr != nil {
		return nil, targetErr
	}
	if target.Status != ChannelStatusDisabled {
		return nil, ErrChannelAdmissionConflict
	}
	original, _ := shadowResourceRequest(r)
	if !shadowTargetMatches(original.Target.Projection, target) {
		return nil, ErrChannelAdmissionConflict
	}
	if sealErr := shadowNoSealEffects(tx, r.OperationID); sealErr != nil {
		return nil, sealErr
	}
	if r.LifecycleState == "cancelled" {
		return shadowLifecycleReceipt(r), nil
	}
	now, e := channelAdmissionTime(tx)
	if e != nil {
		return nil, e
	}
	if now.UnixMilli() < r.CreatedAtMs || now.UnixMilli() < r.PreparedAtMs {
		return nil, ErrChannelAdmissionConflict
	}
	if e = shadowAdvanceRevision(tx, c); e != nil {
		return nil, e
	}
	old := r.LifecycleState
	res := tx.Model(r).Where("lifecycle_state=? AND checkpoint_hash=?", old, r.CheckpointHash).Updates(map[string]any{"lifecycle_state": "cancelled", "lifecycle_revision": c.Revision, "cancelled_at_ms": now.UnixMilli()})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected != 1 {
		return nil, ErrChannelAdmissionConflict
	}
	var after ChannelMigrationShadowResource
	if e = tx.First(&after, "operation_id=?", r.OperationID).Error; e != nil {
		return nil, e
	}
	if _, e = shadowResourceRequest(&after); e != nil {
		return nil, e
	}
	if after.RequestHash != requestHash || after.CheckpointHash != checkpointHash || after.LifecycleState != "cancelled" || after.LifecycleRevision != c.Revision {
		return nil, ErrChannelAdmissionConflict
	}
	if e = shadowLifecycleValid(&after); e != nil {
		return nil, e
	}
	target, e = storedAdmissionIdentity(tx, shadowIdentity(&after))
	if e != nil {
		return nil, e
	}
	if target.Status != ChannelStatusDisabled || !shadowTargetMatches(original.Target.Projection, target) {
		return nil, ErrChannelAdmissionConflict
	}
	if e = shadowNoSealEffects(tx, r.OperationID); e != nil {
		return nil, e
	}
	if after.PreparedRequestHash != "" {
		durable, e := loadMigration(tx, r.OperationID)
		if e != nil {
			return nil, e
		}
		var req ChannelMigrationRequest
		if json.Unmarshal([]byte(durable.RequestJSON), &req) != nil {
			return nil, ErrChannelAdmissionConflict
		}
		if e = validatePreparedShadowLink(tx, c, durable, req, false); e != nil {
			return nil, e
		}
	} else if e = shadowNoOperationEffects(tx, r.OperationID); e != nil {
		return nil, e
	}
	return shadowLifecycleReceipt(&after), nil
}

// CancelMigrationShadow records terminal intent only, never resource disposal.
// No issuer is needed to stop this operation. It cannot cancel a sealed intent.
func (s *ChannelAdmissionStore) CancelMigrationShadow(ctx context.Context, op, requestHash, checkpointHash string) (out *MigrationShadowLifecycleReceipt, err error) {
	if !ValidMigrationReadOperationID(op) || !fullIdentityDigest(requestHash) || !fullIdentityDigest(checkpointHash) {
		return nil, ErrChannelAdmissionConflict
	}
	err = s.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		if e := migrationStorageReady(tx); e != nil {
			return e
		}
		r, e := loadShadowResource(tx, op, 0, false)
		if e != nil {
			return e
		}
		if r == nil {
			return ErrChannelAdmissionConflict
		}
		out, e = cancelShadowTx(tx, c, r, requestHash, checkpointHash)
		return e
	})
	return
}

// This diagnostic read returns only a durable exact lifecycle, never absence as
// permission to restart. It performs no DML and does not certify resource drain.
func (s *ChannelAdmissionStore) ReadMigrationShadowLifecycle(ctx context.Context, op, checkpointHash string) (out *MigrationShadowLifecycleReceipt, err error) {
	if !ValidMigrationReadOperationID(op) || !fullIdentityDigest(checkpointHash) {
		return nil, ErrChannelAdmissionConflict
	}
	result := s.migrationReadTransaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) (MigrationReadResult, error) {
		r, e := loadShadowResource(tx, op, 0, true)
		if e != nil {
			return MigrationReadResult{}, e
		}
		if r == nil || r.CheckpointHash != checkpointHash || !shadowContextMatches(c, r) {
			return MigrationReadResult{}, ErrChannelAdmissionConflict
		}
		if r.PreparedRequestHash != "" {
			p, e := loadMigration(tx, op, true)
			if e != nil {
				return MigrationReadResult{}, e
			}
			var req ChannelMigrationRequest
			if json.Unmarshal([]byte(p.RequestJSON), &req) != nil {
				return MigrationReadResult{}, ErrChannelAdmissionConflict
			}
			if e = validatePreparedShadowLink(tx, c, p, req, true); e != nil {
				return MigrationReadResult{}, e
			}
		} else {
			if e = shadowNoOperationEffects(tx, op); e != nil {
				return MigrationReadResult{}, e
			}
		}
		out = shadowLifecycleReceipt(r)
		return MigrationReadResult{State: "ready"}, nil
	})
	if result.State != "ready" {
		out = nil
		err = ErrChannelAdmissionUnavailable
	}
	return
}

// Control serializes compliant membership writers; ordered locks also observe
// already-running row changes before freezing the complete source checkpoint.
func lockShadowSourceRows(tx *gorm.DB) error {
	var ids []int
	if e := tx.Unscoped().Model(&Channel{}).Clauses(clause.Locking{Strength: "UPDATE"}).Order("id").Limit(MigrationInventoryMaxRows+1).Pluck("id", &ids).Error; e != nil {
		return e
	}
	if len(ids) > MigrationInventoryMaxRows {
		return ErrChannelAdmissionUnavailable
	}
	var models []string
	if e := tx.Model(&ModelConfig{}).Clauses(clause.Locking{Strength: "UPDATE"}).Order("model").Limit(MigrationInventoryMaxRows+1).Pluck("model", &models).Error; e != nil {
		return e
	}
	if len(models) > MigrationInventoryMaxRows {
		return ErrChannelAdmissionUnavailable
	}
	return nil
}
