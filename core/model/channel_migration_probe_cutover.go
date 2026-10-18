package model

import (
	"context"
	"encoding/json"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MigrationProbeCutoverBinding identifies previously accepted server facts. It
// contains no command, proof, replacement deadline, or configuration payload.
type MigrationProbeCutoverBinding struct {
	Version           int    `json:"version"`
	AuthorizationHash string `json:"authorizationHash"`
	RequestHash       string `json:"requestHash"`
	ProbeFactHash     string `json:"probeFactHash"`
	StorageGeneration string `json:"storageGeneration"`
}

func (b MigrationProbeCutoverBinding) Valid() bool {
	return b.Version == 1 && fullIdentityDigest(b.AuthorizationHash) && fullIdentityDigest(b.RequestHash) && fullIdentityDigest(b.ProbeFactHash) && b.StorageGeneration != "" && len(b.StorageGeneration) <= 256
}

// cutoverProbeFacts is pure DB validation. Writers hold control, resource, run,
// task, use, then prepared, in that order. No network or billing refresh occurs.
func cutoverProbeFacts(tx *gorm.DB, c *ChannelAdmissionControl, op string, b MigrationProbeCutoverBinding, lock bool) (ChannelMigrationRequest, VerifiedMigrationProbe, error) {
	var zero ChannelMigrationRequest
	var fact VerifiedMigrationProbe
	if e := probeRuntimeStorageReady(tx); e != nil {
		return zero, fact, e
	}
	resource, e := migrationTargetByOperation(tx, op, !lock)
	if e != nil || resource == nil || resource.LifecycleState != "consumed" {
		return zero, fact, ErrChannelAdmissionConflict
	}
	run, a, _, e := loadProbeRun(tx, op, !lock)
	if e != nil {
		return zero, fact, e
	}
	if !validMigrationResourceKind(a.Command) || resource.Kind != a.Command.TargetResourceKind || resource.RequestHash != run.ResourceRequestHash || resource.CheckpointHash != run.CheckpointHash || resource.Target != a.Command.Seal.To || run.AuthorizationHash != b.AuthorizationHash || run.CommandHash != b.RequestHash || run.StorageGeneration != b.StorageGeneration || run.StorageGeneration != c.StorageGeneration || run.EvidenceJSON == "" || run.UseID == "" {
		return zero, fact, ErrChannelAdmissionConflict
	}
	q := tx
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var task NativeTask
	if e = q.Session(&gorm.Session{}).First(&task, "id=?", run.TaskID).Error; e != nil {
		return zero, fact, e
	}
	var use ChannelAdmissionUse
	if e = q.Session(&gorm.Session{}).First(&use, "id=?", run.UseID).Error; e != nil {
		return zero, fact, e
	}
	observed, e := probeObservationTx(tx, c, op)
	if e != nil {
		return zero, fact, e
	}
	if observed.State != "pending" || observed.Observation == nil || observed.Observation.TaskStatus != "completed" || probeTaskFactHash(&task) != run.EvidenceTaskHash || probeDeliveryAndSettlement(&task) != nil {
		return zero, fact, ErrChannelAdmissionConflict
	}
	if json.Unmarshal([]byte(run.EvidenceJSON), &fact) != nil || fact.RequestHash != run.CommandHash || fact.StorageGeneration != run.StorageGeneration {
		return zero, fact, ErrChannelAdmissionConflict
	}
	hash, e := admissionHash(fact)
	if e != nil || hash != b.ProbeFactHash {
		return zero, fact, ErrChannelAdmissionConflict
	}
	row, e := loadMigration(tx, op, !lock)
	if e != nil {
		return zero, fact, e
	}
	if (row.State != "prepared" && row.State != "committed") || row.RequestHash != b.RequestHash || row.ProbeFactHash != hash || row.ProbeHash != fact.EvidenceHash || row.StorageGeneration != b.StorageGeneration || row.ShadowCheckpointHash != resource.CheckpointHash {
		return zero, fact, ErrChannelAdmissionConflict
	}
	return a.Command, fact, nil
}

// CutoverMigrationFromProbe never dispatches or refreshes a probe. The initial
// read only loads the frozen command; the transaction rechecks all original
// facts and current authorization before and after the atomic mutation.
func (s *ChannelAdmissionStore) CutoverMigrationFromProbe(ctx context.Context, op string, b MigrationProbeCutoverBinding) (*ChannelMigrationPrepared, error) {
	if !ValidMigrationReadOperationID(op) || !b.Valid() {
		return nil, ErrChannelAdmissionConflict
	}
	var command ChannelMigrationRequest
	var fact VerifiedMigrationProbe
	var readErr error
	read := s.migrationReadTransaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) (MigrationReadResult, error) {
		command, fact, readErr = cutoverProbeFacts(tx, c, op, b, false)
		return migrationReadState("ready", ""), readErr
	})
	if read.State != "ready" {
		if readErr != nil {
			return nil, readErr
		}
		return nil, ErrChannelAdmissionUnavailable
	}
	// Resource kind comes from the persisted authorization, never the HTTP body.
	if !validMigrationResourceKind(command) {
		return nil, ErrChannelAdmissionConflict
	}
	copyStore := *s
	copyStore.LoadMigrationProbe = func(_ context.Context, r ChannelMigrationRequest) (VerifiedMigrationProbe, error) {
		h, _ := admissionHash(r)
		if h != b.RequestHash {
			return VerifiedMigrationProbe{}, ErrChannelAdmissionConflict
		}
		return fact, nil
	}
	return copyStore.cutoverPrepared(ctx, command, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		_, current, e := cutoverProbeFacts(tx, c, op, b, true)
		if e != nil {
			return e
		}
		if current != fact {
			return ErrChannelAdmissionConflict
		}
		return nil
	})
}
