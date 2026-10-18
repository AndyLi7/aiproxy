package model

import (
	"encoding/json"

	"gorm.io/gorm"
)

// Original billing is a snapshot join, not probe authorization. In particular,
// cancellation or a later legitimate cutover does not erase the paid task's
// original resource. Reverse does not read current family/pin/projection or proof.
func probeBillingOriginalResourceTx(tx *gorm.DB, c *ChannelAdmissionControl, run *ChannelMigrationProbeRun, a MigrationProbeAuthorization, spec MigrationProbeSpec) (string, error) {
	target, e := migrationTargetByOperation(tx, run.OperationID, true)
	if e != nil {
		return "", e
	}
	if target == nil || target.Kind != a.Command.TargetResourceKind || target.Epoch != c.Epoch || target.StorageGeneration != c.StorageGeneration || target.Revision > c.Revision || target.RequestHash != run.ResourceRequestHash || target.CheckpointHash != run.CheckpointHash || target.FromChannelID != a.Command.Seal.From.ChannelID || target.Target != a.Command.Seal.To {
		return "", ErrChannelAdmissionConflict
	}
	if !reverseMigration(a.Command) {
		// Preserve the forward lifecycle/read contract, including its original projection checks.
		life, e := readShadowLifecycleFactsTx(tx, c, run.OperationID, true)
		if e != nil {
			return "", e
		}
		if life.State != "ready" || life.Observation == nil {
			return "", ErrChannelAdmissionConflict
		}
		creation := life.Observation.Creation
		if run.ResourceRequestHash != creation.RequestHash || run.CheckpointHash != creation.CheckpointHash || a.Command.Seal.From != creation.From || a.Command.Seal.To != creation.Shadow || a.Command.ToCredentialID != life.Observation.ResourceRequest.Target.CredentialID || life.Observation.ResourceRequest.Target.Projection.ModelMapping[a.Model] != spec.Endpoint || !shadowModelsExist(life.Observation.ResourceRequest.Target.Projection.Models, []string{a.Model}) {
			return "", ErrChannelAdmissionConflict
		}
		return life.Observation.Lifecycle.State, nil
	}
	r, e := loadRetiredResource(tx, run.OperationID, true)
	if e != nil {
		return "", e
	}
	q, e := retiredRequest(r)
	if e != nil {
		return "", e
	}
	commandHash, _ := admissionHash(q.Command)
	if commandHash != run.CommandHash || r.RequestHash != a.ResourceRequestHash || r.CheckpointHash != a.CheckpointHash || r.CreatedAtMs > run.CreatedAtMs {
		return "", ErrChannelAdmissionConflict
	}
	if r.PreparedRequestHash != "" {
		p, e := loadMigration(tx, run.OperationID, true)
		if e != nil {
			return "", e
		}
		if e = validateRetiredPreparedLink(tx, c, p, q.Command, true); e != nil {
			return "", e
		}
	} else if e = shadowNoOperationEffects(tx, run.OperationID); e != nil {
		return "", e
	}
	if r.LifecycleState == "cancelled" {
		if e = shadowNoSealEffects(tx, run.OperationID); e != nil {
			return "", e
		}
	}
	if e = probeBillingRetirementTx(tx, c, r, q.Command); e != nil {
		return "", e
	}
	// Reverse resources bind the current reviewed projection hash, not a fabricated
	// historical model map. Begin froze this exact compiled spec after validating
	// the current target; the run/spec/task/use hashes retain that original evidence.
	return r.LifecycleState, nil
}

// Read the recorded retirement, never ORDER BY latest or migrationCurrentHead:
// a subsequent legal reverse must not make this original bill unrecognizable.
func probeBillingRetirementTx(tx *gorm.DB, c *ChannelAdmissionControl, r *ChannelMigrationRetiredTargetResource, req ChannelMigrationRequest) error {
	var b ChannelAdmissionBarrier
	if e := tx.First(&b, "operation_id=?", r.RetirementOperationID).Error; e != nil {
		return e
	}
	if b.OperationID != req.Seal.RevertOf || b.Version != ChannelAdmissionProtocolVersion || b.Epoch != c.Epoch || b.Revision != r.RetirementRevision || b.Revision > r.SourceRevision || b.RequestHash != r.RetirementRequestHash || b.RecordedAtMs <= 0 || b.FromChannelID != req.Seal.To.ChannelID || b.FromKeyDigest != req.Seal.To.KeyDigest || b.FromIdentityHash != req.Seal.To.IdentityHash || b.ToChannelID != req.Seal.From.ChannelID || b.ToKeyDigest != req.Seal.From.KeyDigest || b.ToIdentityHash != req.Seal.From.IdentityHash {
		return ErrChannelAdmissionConflict
	}
	old, e := loadMigration(tx, b.OperationID, true)
	if e != nil {
		return e
	}
	var original ChannelMigrationRequest
	if json.Unmarshal([]byte(old.RequestJSON), &original) != nil {
		return ErrChannelAdmissionConflict
	}
	hash, _ := admissionHash(original.Seal)
	if old.State != "committed" || old.SealRevision != b.Revision || old.CommitRevision > r.SourceRevision || old.StorageGeneration != c.StorageGeneration || original.Seal.Epoch != b.Epoch || hash != b.RequestHash || original.Seal.From != req.Seal.To || original.Seal.To != req.Seal.From || original.FromCredentialID != req.ToCredentialID || original.ToCredentialID != req.FromCredentialID {
		return ErrChannelAdmissionConflict
	}
	return validatePreparedTargetLink(tx, c, old, original, true)
}
