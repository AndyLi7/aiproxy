package model

import (
	"context"
	"encoding/json"
	"errors"

	"gorm.io/gorm"
)

func (s *ChannelAdmissionStore) PrepareReverseMigration(ctx context.Context, input ChannelMigrationRequest) (result *ChannelMigrationPrepared, err error) {
	if !reverseMigration(input) || !validProbeCommand(input) || !ValidMigrationReadOperationID(input.Seal.OperationID) {
		return nil, ErrChannelAdmissionConflict
	}
	a, e := s.migrationAuthorization(ctx, input)
	if e != nil {
		return nil, e
	}
	err = s.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		r, e := loadRetiredResource(tx, input.Seal.OperationID, false)
		if e != nil {
			return e
		}
		old, e := loadMigration(tx, input.Seal.OperationID)
		if e == nil {
			if e = validateMigrationAuthorization(tx, c, a, "seal", s.MigrationFeatures); e != nil {
				return e
			}
			if _, e = lockDeploymentProof(tx, c, a.deployment, s.Executor); e != nil {
				return e
			}
			result = old
			return finalizeMigrationProbe(tx, a.probe)
		}
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		if r.LifecycleState != "created" {
			return ErrChannelAdmissionConflict
		}
		if e = lockShadowSourceRows(tx); e != nil {
			return e
		}
		if e = retiredCurrent(tx, c, r, input, s.MigrationFeatures); e != nil {
			return e
		}
		if e = shadowNoOperationEffects(tx, input.Seal.OperationID); e != nil {
			return e
		}
		ids := []int{input.Seal.From.ChannelID, input.Seal.To.ChannelID}
		var n int64
		if e = tx.Model(&ChannelMigrationPrepared{}).Where("state IN ? AND (from_channel_id IN ? OR to_channel_id IN ?)", []string{"prepared", "sealed"}, ids, ids).Count(&n).Error; e != nil {
			return e
		}
		if n != 0 {
			return ErrChannelAdmissionConflict
		}
		now, e := lockDeploymentProof(tx, c, a.deployment, s.Executor)
		if e != nil {
			return e
		}
		if now.UnixMilli() < r.CreatedAtMs {
			return ErrChannelAdmissionConflict
		}
		raw, _ := json.Marshal(input)
		ph, _ := a.probe.Fingerprint()
		row := ChannelMigrationPrepared{ShadowCheckpointHash: r.CheckpointHash, OperationID: r.OperationID, RequestJSON: string(raw), RequestHash: a.hash, StorageGeneration: c.StorageGeneration, ProbeHash: a.probe.EvidenceHash, ProbeFactHash: ph, State: "prepared", FromChannelID: ids[0], ToChannelID: ids[1]}
		if e = tx.Create(&row).Error; e != nil {
			return e
		}
		if e = shadowAdvanceRevision(tx, c); e != nil {
			return e
		}
		res := tx.Model(r).Where("lifecycle_state='created' AND checkpoint_hash=?", r.CheckpointHash).Updates(map[string]any{"lifecycle_state": "consumed", "lifecycle_revision": c.Revision, "prepared_request_hash": row.RequestHash, "prepared_probe_hash": row.ProbeHash, "prepared_probe_fact_hash": row.ProbeFactHash, "prepared_at_ms": now.UnixMilli()})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrChannelAdmissionConflict
		}
		if e = validateRetiredPreparedLink(tx, c, &row, input, false); e != nil {
			return e
		}
		if e = finalizeMigrationProbe(tx, a.probe); e != nil {
			return e
		}
		if e = finalizeAdmissionProof(tx, c, a.deployment, c.Revision); e != nil {
			return e
		}
		if _, e = lockDeploymentProof(tx, c, a.deployment, s.Executor); e != nil {
			return e
		}
		if e = finalizeMigrationProbe(tx, a.probe); e != nil {
			return e
		}
		result = &row
		return nil
	})
	return
}

// Cancellation is durable terminal intent, never a status/key mutation. Only
// unsealed reverse intents can be cancelled; old sealed rows are not adopted.
func (s *ChannelAdmissionStore) CancelRetiredTargetResource(ctx context.Context, op, requestHash, checkpointHash string) (out *ChannelMigrationRetiredTargetResource, err error) {
	if !ValidMigrationReadOperationID(op) || !fullIdentityDigest(requestHash) || !fullIdentityDigest(checkpointHash) {
		return nil, ErrChannelAdmissionConflict
	}
	err = s.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		r, e := loadRetiredResource(tx, op, false)
		if e != nil {
			return e
		}
		q, e := retiredRequest(r)
		if e != nil {
			return e
		}
		if r.RequestHash != requestHash || r.CheckpointHash != checkpointHash || r.Epoch != c.Epoch || r.StorageGeneration != c.StorageGeneration || r.LifecycleRevision > c.Revision {
			return ErrChannelAdmissionConflict
		}
		if e = shadowNoSealEffects(tx, op); e != nil {
			return e
		}
		// The target remains enabled for old tasks; cancellation cannot restore a
		// mutated identity or current pin snapshot, and need not require current head.
		target, e := storedAdmissionIdentity(tx, q.Command.Seal.To)
		if e != nil {
			return e
		}
		if target.Status != ChannelStatusEnabled {
			return ErrChannelAdmissionConflict
		}
		proj, e := migrationProjection(tx, target.ID)
		if e != nil || proj != q.Command.ProjectionHash {
			return ErrChannelAdmissionConflict
		}
		row, e := loadMigration(tx, op)
		if errors.Is(e, gorm.ErrRecordNotFound) {
			if r.PreparedRequestHash != "" || r.LifecycleState == "consumed" {
				return ErrChannelAdmissionConflict
			}
		} else if e != nil {
			return e
		} else {
			if e = validateRetiredPreparedLink(tx, c, row, q.Command, false); e != nil {
				return e
			}
			if row.State != "prepared" && row.State != "abandoned" {
				return ErrChannelAdmissionConflict
			}
			if row.State == "prepared" {
				res := tx.Model(row).Where("state='prepared' AND request_hash=?", r.PreparedRequestHash).UpdateColumn("state", "abandoned")
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected != 1 {
					return ErrChannelAdmissionConflict
				}
			}
		}
		if r.LifecycleState == "cancelled" {
			out = r
			return nil
		}
		now, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		if now.UnixMilli() < r.CreatedAtMs || now.UnixMilli() < r.PreparedAtMs {
			return ErrChannelAdmissionConflict
		}
		if e = shadowAdvanceRevision(tx, c); e != nil {
			return e
		}
		res := tx.Model(r).Where("lifecycle_state=? AND checkpoint_hash=?", r.LifecycleState, r.CheckpointHash).Updates(map[string]any{"lifecycle_state": "cancelled", "lifecycle_revision": c.Revision, "cancelled_at_ms": now.UnixMilli()})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrChannelAdmissionConflict
		}
		out, e = loadRetiredResource(tx, op, false)
		if e != nil {
			return e
		}
		if out.LifecycleState != "cancelled" || out.LifecycleRevision != c.Revision {
			return ErrChannelAdmissionConflict
		}
		if row != nil {
			row, e = loadMigration(tx, op)
			if e != nil {
				return e
			}
			if e = validateRetiredPreparedLink(tx, c, row, q.Command, false); e != nil {
				return e
			}
		}
		return shadowNoSealEffects(tx, op)
	})
	return
}

// Returns only this exact durable resource; absence never grants restart. The
// prepared reader separately verifies current head and postconditions.
func (s *ChannelAdmissionStore) ReadRetiredTargetResource(ctx context.Context, op, checkpoint string) (out *ChannelMigrationRetiredTargetResource, err error) {
	if !ValidMigrationReadOperationID(op) || !fullIdentityDigest(checkpoint) {
		return nil, ErrChannelAdmissionConflict
	}
	result := s.migrationReadTransaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) (MigrationReadResult, error) {
		r, e := loadRetiredResource(tx, op, true)
		if e != nil {
			return MigrationReadResult{}, e
		}
		if r.CheckpointHash != checkpoint || r.Epoch != c.Epoch || r.StorageGeneration != c.StorageGeneration || r.LifecycleRevision > c.Revision {
			return MigrationReadResult{}, ErrChannelAdmissionConflict
		}
		q, _ := retiredRequest(r)
		if r.PreparedRequestHash != "" {
			p, e := loadMigration(tx, op, true)
			if e != nil {
				return MigrationReadResult{}, e
			}
			if e = validateRetiredPreparedLink(tx, c, p, q.Command, true); e != nil {
				return MigrationReadResult{}, e
			}
		} else if e = shadowNoOperationEffects(tx, op); e != nil {
			return MigrationReadResult{}, e
		}
		out = r
		return migrationReadState("ready", ""), nil
	})
	if result.State != "ready" {
		return nil, ErrChannelAdmissionUnavailable
	}
	return
}

// A read-only binding view for the two explicit resource kinds. Never persisted
// as a shadow and never accepted as authorization; each caller validates its kind.
type migrationTargetBinding struct {
	Kind              string
	OperationID       string
	RequestHash       string
	CheckpointHash    string
	StorageGeneration string
	Epoch             string
	Revision          int64
	FromChannelID     int
	Target            ChannelIdentity
	LifecycleState    string
}

func migrationTargetByOperation(tx *gorm.DB, op string, snapshot bool) (*migrationTargetBinding, error) {
	present, e := retiredStoragePresent(tx)
	if e != nil {
		return nil, e
	}
	var reverse *ChannelMigrationRetiredTargetResource
	if present {
		reverse, e = loadRetiredResource(tx, op, snapshot)
		if e != nil && !errors.Is(e, gorm.ErrRecordNotFound) {
			return nil, e
		}
	}
	forward, e := loadShadowResource(tx, op, 0, snapshot)
	if e != nil {
		return nil, e
	}
	if reverse != nil {
		if forward != nil {
			return nil, ErrChannelAdmissionConflict
		}
		q, e := retiredRequest(reverse)
		if e != nil {
			return nil, e
		}
		return &migrationTargetBinding{MigrationReuseRetiredTarget, op, reverse.RequestHash, reverse.CheckpointHash, reverse.StorageGeneration, reverse.Epoch, reverse.LifecycleRevision, reverse.FromChannelID, q.Command.Seal.To, reverse.LifecycleState}, nil
	}
	if forward == nil {
		return nil, nil
	}
	return &migrationTargetBinding{"", op, forward.RequestHash, forward.CheckpointHash, forward.StorageGeneration, forward.SourceEpoch, forward.LifecycleRevision, forward.FromChannelID, shadowIdentity(forward), forward.LifecycleState}, nil
}
func reverseProbeResource(tx *gorm.DB, c *ChannelAdmissionControl, a MigrationProbeAuthorization, features []string) error {
	r, e := loadRetiredResource(tx, a.Command.Seal.OperationID, false)
	if e != nil {
		return e
	}
	if r.LifecycleState != "created" || r.RequestHash != a.ResourceRequestHash || r.CheckpointHash != a.CheckpointHash {
		return ErrChannelAdmissionConflict
	}
	if e = retiredCurrent(tx, c, r, a.Command, features); e != nil {
		return e
	}
	return probeOwner(tx, a)
}
