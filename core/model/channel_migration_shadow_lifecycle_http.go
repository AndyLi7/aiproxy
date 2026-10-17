package model

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"gorm.io/gorm"
)

// These facts describe resource lifecycle only. A consumed observation is not
// a current-family-head grant or permission to execute, seal, drain or delete.
type MigrationShadowLifecycleResult struct {
	Version           int                                  `json:"version"`
	Source            string                               `json:"source"`
	State             string                               `json:"state"`
	Reason            string                               `json:"reason"`
	OperationID       string                               `json:"operationId"`
	Epoch             string                               `json:"epoch"`
	StorageGeneration string                               `json:"storageGeneration"`
	CurrentRevision   string                               `json:"currentRevision"`
	Observation       *MigrationShadowLifecycleObservation `json:"observation"`
}
type MigrationShadowLifecycleObservation struct {
	ResourceRequest MigrationShadowRequest          `json:"resourceRequest"`
	Creation        MigrationShadowReceipt          `json:"creation"`
	Lifecycle       MigrationShadowLifecycleFacts   `json:"lifecycle"`
	PreparedBinding *MigrationShadowPreparedBinding `json:"preparedBinding"`
}
type MigrationShadowLifecycleFacts struct {
	State         string `json:"state"`
	Revision      string `json:"revision"`
	PreparedAtMs  string `json:"preparedAtMs"`
	CancelledAtMs string `json:"cancelledAtMs"`
}
type MigrationShadowPreparedBinding struct {
	OperationID          string `json:"operationId"`
	RequestHash          string `json:"requestHash"`
	ProbeHash            string `json:"probeHash"`
	ProbeFactHash        string `json:"probeFactHash"`
	ShadowCheckpointHash string `json:"shadowCheckpointHash"`
	StorageGeneration    string `json:"storageGeneration"`
	FromChannelID        int    `json:"fromChannelId"`
	ToChannelID          int    `json:"toChannelId"`
	State                string `json:"state"`
}

func shadowLifecycleResult(op, state, reason string) MigrationShadowLifecycleResult {
	return MigrationShadowLifecycleResult{Version: 2, Source: "gateway-migration-shadow-lifecycle-v2", State: state, Reason: reason, OperationID: op}
}
func shadowLifecycleContext(op, state string, c *ChannelAdmissionControl) MigrationShadowLifecycleResult {
	r := shadowLifecycleResult(op, state, "")
	r.Epoch = c.Epoch
	r.StorageGeneration = c.StorageGeneration
	r.CurrentRevision = strconv.FormatInt(c.Revision, 10)
	return r
}
func shadowLifecycleResultFailure(op string, e error) MigrationShadowLifecycleResult {
	if errors.Is(e, ErrChannelAdmissionConflict) || errors.Is(e, gorm.ErrRecordNotFound) {
		return shadowLifecycleResult(op, "conflict", "migration_shadow_lifecycle_conflict")
	}
	return shadowLifecycleResult(op, "unavailable", "migration_shadow_lifecycle_unavailable")
}
func readShadowLifecycleFactsTx(tx *gorm.DB, c *ChannelAdmissionControl, op string, snapshot bool) (MigrationShadowLifecycleResult, error) {
	if e := shadowStorageReady(tx); e != nil {
		return MigrationShadowLifecycleResult{}, e
	}
	r, e := loadShadowResource(tx, op, 0, snapshot)
	if e != nil {
		return MigrationShadowLifecycleResult{}, e
	}
	if r == nil {
		// This is current complete storage absence only, never evidence that no
		// history existed before a restore or out-of-band deletion.
		if e = shadowNoOperationEffects(tx, op); e != nil {
			return MigrationShadowLifecycleResult{}, e
		}
		return shadowLifecycleContext(op, "absent", c), nil
	}
	if !shadowContextMatches(c, r) {
		return MigrationShadowLifecycleResult{}, ErrChannelAdmissionConflict
	}
	original, e := shadowResourceRequest(r)
	if e != nil {
		return MigrationShadowLifecycleResult{}, e
	}
	if int64(r.FromChannelID) > 9007199254740991 || int64(r.ShadowChannelID) > 9007199254740991 {
		return MigrationShadowLifecycleResult{}, ErrChannelAdmissionUnavailable
	}
	var binding *MigrationShadowPreparedBinding
	targetStatus := ChannelStatusDisabled
	if r.PreparedRequestHash != "" {
		p, e := loadMigration(tx, op, snapshot)
		if e != nil {
			return MigrationShadowLifecycleResult{}, e
		}
		var req ChannelMigrationRequest
		if json.Unmarshal([]byte(p.RequestJSON), &req) != nil {
			return MigrationShadowLifecycleResult{}, ErrChannelAdmissionConflict
		}
		if e = validatePreparedShadowLink(tx, c, p, req, snapshot); e != nil {
			return MigrationShadowLifecycleResult{}, e
		}
		if p.State == "committed" {
			targetStatus = ChannelStatusEnabled
		}
		binding = &MigrationShadowPreparedBinding{p.OperationID, p.RequestHash, p.ProbeHash, p.ProbeFactHash, p.ShadowCheckpointHash, p.StorageGeneration, p.FromChannelID, p.ToChannelID, p.State}
	} else if e = shadowNoOperationEffects(tx, op); e != nil {
		return MigrationShadowLifecycleResult{}, e
	}
	// Historical source inventory/family may legitimately differ. Verify only
	// this retained physical resource; do not relax or mutate its old checkpoint.
	target, e := storedAdmissionIdentity(tx, shadowIdentity(r), snapshot)
	if e != nil {
		return MigrationShadowLifecycleResult{}, e
	}
	// The original helper deliberately includes disabled status for write paths.
	// This read also observes committed targets; validate their actual status
	// first, then compare the unchanged creation projection on a local copy.
	projectionTarget := *target
	projectionTarget.Status = ChannelStatusDisabled
	if target.Status != targetStatus || !shadowTargetMatches(original.Target.Projection, &projectionTarget) {
		return MigrationShadowLifecycleResult{}, ErrChannelAdmissionConflict
	}
	if r.LifecycleState == "cancelled" {
		if e = shadowNoSealEffects(tx, op); e != nil {
			return MigrationShadowLifecycleResult{}, e
		}
	}
	// The original request is a bounded, typed, non-secret review projection,
	// not a channel dump. Recheck current known credentials before returning it;
	// the caller compares this original request without guessing Go hashes.
	var sourceKeys []string
	if e = tx.Unscoped().Model(&Channel{}).Order("id").Limit(MigrationInventoryMaxRows+1).Pluck("key", &sourceKeys).Error; e != nil {
		return MigrationShadowLifecycleResult{}, e
	}
	if len(sourceKeys) > MigrationInventoryMaxRows || !inventorySafeJSON([]byte(r.RequestJSON), sourceKeys, 0) {
		return MigrationShadowLifecycleResult{}, ErrChannelAdmissionUnavailable
	}
	out := shadowLifecycleContext(op, "ready", c)
	out.Observation = &MigrationShadowLifecycleObservation{
		ResourceRequest: original,
		Creation:        MigrationShadowReceipt{r.OperationID, r.RequestHash, r.SourceSnapshotHash, r.SourceEpoch, r.StorageGeneration, strconv.FormatInt(r.CreatedRevision, 10), strconv.FormatInt(r.CreatedAtMs, 10), original.Source.From, shadowIdentity(r), r.ShadowProjectionHash, r.CheckpointHash},
		Lifecycle:       MigrationShadowLifecycleFacts{r.LifecycleState, strconv.FormatInt(r.LifecycleRevision, 10), strconv.FormatInt(r.PreparedAtMs, 10), strconv.FormatInt(r.CancelledAtMs, 10)},
		PreparedBinding: binding,
	}
	return out, nil
}

// ReadMigrationShadowLifecycleFacts produces the complete envelope in one
// no-DML consistent snapshot. Error paths discard partial facts, never absent.
func (s *ChannelAdmissionStore) ReadMigrationShadowLifecycleFacts(ctx context.Context, op string) MigrationShadowLifecycleResult {
	if !ValidMigrationReadOperationID(op) {
		return shadowLifecycleResult(op, "conflict", "invalid_operation_id")
	}
	var out MigrationShadowLifecycleResult
	result := s.migrationReadTransaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) (MigrationReadResult, error) {
		if e := shadowStorageReady(tx); e != nil {
			return migrationReadState("unsupported", "migration_shadow_lifecycle_storage_unavailable"), nil
		}
		var e error
		out, e = readShadowLifecycleFactsTx(tx, c, op, true)
		return MigrationReadResult{State: "ready"}, e
	})
	if result.State != "ready" {
		return shadowLifecycleResult(op, result.State, "migration_shadow_lifecycle_"+result.State)
	}
	return out
}

// CancelMigrationShadowFacts returns its complete observation in the same
// transaction as cancellation. The controller must not stitch subsequent reads.
func (s *ChannelAdmissionStore) CancelMigrationShadowFacts(ctx context.Context, op, requestHash, checkpointHash string) MigrationShadowLifecycleResult {
	if !ValidMigrationReadOperationID(op) || !fullIdentityDigest(requestHash) || !fullIdentityDigest(checkpointHash) {
		return shadowLifecycleResult(op, "conflict", "invalid_migration_shadow_cancel")
	}
	var out MigrationShadowLifecycleResult
	e := s.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		if e := migrationStorageReady(tx); e != nil {
			return e
		}
		if e := shadowStorageReady(tx); e != nil {
			return e
		}
		r, e := loadShadowResource(tx, op, 0, false)
		if e != nil {
			return e
		}
		if r == nil {
			return ErrChannelAdmissionConflict
		}
		if _, e = cancelShadowTx(tx, c, r, requestHash, checkpointHash); e != nil {
			return e
		}
		out, e = readShadowLifecycleFactsTx(tx, c, op, false)
		return e
	})
	if e != nil {
		return shadowLifecycleResultFailure(op, e)
	}
	return out
}
