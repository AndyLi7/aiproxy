package model

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

// Explicit resource facts, not a shadow or an execution/disable authorization.
// Decimal wire values preserve revisions without Javascript rounding.
type MigrationRetiredResourceResult struct {
	Version           int                                  `json:"version"`
	Source            string                               `json:"source"`
	State             string                               `json:"state"`
	Reason            string                               `json:"reason"`
	OperationID       string                               `json:"operationId"`
	Epoch             string                               `json:"epoch"`
	StorageGeneration string                               `json:"storageGeneration"`
	CurrentRevision   string                               `json:"currentRevision"`
	Observation       *MigrationRetiredResourceObservation `json:"observation"`
}
type MigrationRetiredResourceRequest struct {
	Version            int                  `json:"version"`
	Command            MigrationReadRequest `json:"command"`
	SourceSnapshotHash string               `json:"sourceSnapshotHash"`
}
type MigrationRetiredResourceObservation struct {
	ResourceRequest       MigrationRetiredResourceRequest  `json:"resourceRequest"`
	RequestHash           string                           `json:"requestHash"`
	CheckpointHash        string                           `json:"checkpointHash"`
	Target                ChannelIdentity                  `json:"target"`
	CreatedRevision       string                           `json:"createdRevision"`
	CreatedAtMs           string                           `json:"createdAtMs"`
	RetirementRevision    string                           `json:"retirementRevision"`
	RetirementRequestHash string                           `json:"retirementRequestHash"`
	Lifecycle             MigrationShadowLifecycleFacts    `json:"lifecycle"`
	PreparedBinding       *MigrationRetiredPreparedBinding `json:"preparedBinding"`
}
type MigrationRetiredPreparedBinding struct {
	OperationID            string `json:"operationId"`
	RequestHash            string `json:"requestHash"`
	ProbeHash              string `json:"probeHash"`
	ProbeFactHash          string `json:"probeFactHash"`
	ResourceCheckpointHash string `json:"resourceCheckpointHash"`
	StorageGeneration      string `json:"storageGeneration"`
	FromChannelID          int    `json:"fromChannelId"`
	ToChannelID            int    `json:"toChannelId"`
	State                  string `json:"state"`
}

func retiredHTTPResult(op, state string) MigrationRetiredResourceResult {
	return MigrationRetiredResourceResult{
		Version:     1,
		Source:      "gateway-migration-retired-target-v1",
		OperationID: op,
		State:       state,
	}
}

func retiredHTTPFailure(op string, e error) MigrationRetiredResourceResult {
	r := retiredHTTPResult(op, "unavailable")
	if errors.Is(e, ErrChannelAdmissionConflict) || errors.Is(e, gorm.ErrRecordNotFound) {
		r.State = "conflict"
	}

	r.Reason = "migration_retired_target_" + r.State

	return r
}

func retiredWireRequest(q MigrationRetiredTargetRequest) MigrationRetiredResourceRequest {
	r := q.Command

	return MigrationRetiredResourceRequest{
		q.Version,
		MigrationReadRequest{
			r.TargetResourceKind,
			MigrationReadSealRequest{
				r.Seal.OperationID,
				r.Seal.Epoch,
				migrationDecimal(r.Seal.ExpectedRevision),
				r.Seal.From,
				r.Seal.To,
				r.Seal.RevertOf,
			},
			r.FromCredentialID,
			r.ToCredentialID,
			r.ExpectedPinHash,
			r.ProjectionHash,
			r.FamilyHash,
		},
		q.SourceSnapshotHash,
	}
}

// The original request hash and complete lifecycle share a read snapshot.
// Lost create replies never require guessing the checkpoint or authorizing retry.
func (s *ChannelAdmissionStore) readRetiredHTTPFacts(
	ctx context.Context,
	op, requestHash, checkpoint string,
) MigrationRetiredResourceResult {
	var out MigrationRetiredResourceResult

	result := s.migrationReadTransaction(
		ctx,
		func(tx *gorm.DB, c *ChannelAdmissionControl) (MigrationReadResult, error) {
			r, e := loadRetiredResource(tx, op, true)
			if errors.Is(e, gorm.ErrRecordNotFound) {
				if e = retentionNoOperationEffects(tx, op); e != nil {
					return MigrationReadResult{}, e
				}

				if e = migrationReadNoOperationEffects(tx, op); e != nil {
					return MigrationReadResult{}, e
				}

				out = retiredHTTPResult(op, "absent")
			} else {
				if e != nil {
					return MigrationReadResult{}, e
				}

				if r.RequestHash != requestHash ||
					(checkpoint != "" && r.CheckpointHash != checkpoint) ||
					r.Epoch != c.Epoch ||
					r.StorageGeneration != c.StorageGeneration ||
					r.LifecycleRevision > c.Revision {
					return MigrationReadResult{}, ErrChannelAdmissionConflict
				}

				q, e := retiredRequest(r)
				if e != nil {
					return MigrationReadResult{}, e
				}
				// Historical resource evidence is separate from the current family head.
				shadow, e := loadShadowResource(tx, op, 0, true)
				if e != nil {
					return MigrationReadResult{}, e
				}

				if shadow != nil {
					return MigrationReadResult{}, ErrChannelAdmissionConflict
				}

				if e = probeBillingRetirementTx(tx, c, r, q.Command); e != nil {
					return MigrationReadResult{}, e
				}

				var binding *MigrationRetiredPreparedBinding
				if r.PreparedRequestHash != "" {
					p, e := loadMigration(tx, op, true)
					if e != nil {
						return MigrationReadResult{}, e
					}

					if e = validateRetiredPreparedLink(tx, c, p, q.Command, true); e != nil {
						return MigrationReadResult{}, e
					}

					binding = &MigrationRetiredPreparedBinding{
						p.OperationID,
						p.RequestHash,
						p.ProbeHash,
						p.ProbeFactHash,
						p.ShadowCheckpointHash,
						p.StorageGeneration,
						p.FromChannelID,
						p.ToChannelID,
						p.State,
					}
				} else if e = shadowNoOperationEffects(tx, op); e != nil {
					return MigrationReadResult{}, e
				}

				if r.LifecycleState == "cancelled" {
					if e = shadowNoSealEffects(tx, op); e != nil {
						return MigrationReadResult{}, e
					}
				}

				out = retiredHTTPResult(op, "ready")
				out.Observation = &MigrationRetiredResourceObservation{
					retiredWireRequest(q),
					r.RequestHash,
					r.CheckpointHash,
					q.Command.Seal.To,
					migrationDecimal(r.CreatedRevision),
					migrationDecimal(r.CreatedAtMs),
					migrationDecimal(r.RetirementRevision),
					r.RetirementRequestHash,
					MigrationShadowLifecycleFacts{
						r.LifecycleState,
						migrationDecimal(r.LifecycleRevision),
						migrationDecimal(r.PreparedAtMs),
						migrationDecimal(r.CancelledAtMs),
					},
					binding,
				}
			}

			out.Epoch = c.Epoch
			out.StorageGeneration = c.StorageGeneration
			out.CurrentRevision = migrationDecimal(c.Revision)

			return migrationReadState("ready", ""), nil
		},
	)
	if result.State != "ready" {
		out = retiredHTTPResult(op, result.State)
		out.Reason = "migration_retired_target_" + result.State
	}

	return out
}

func validRetiredHTTPRequest(q MigrationRetiredTargetRequest) bool {
	return int64(q.Command.Seal.From.ChannelID) <= 9007199254740991 &&
		int64(q.Command.Seal.To.ChannelID) <= 9007199254740991 &&
		q.Version == 1 &&
		validProbeCommand(q.Command) &&
		reverseMigration(q.Command) &&
		ValidMigrationReadOperationID(q.Command.Seal.OperationID) &&
		fullIdentityDigest(q.SourceSnapshotHash)
}

func (s *ChannelAdmissionStore) ReadMigrationRetiredTargetFacts(
	ctx context.Context,
	q MigrationRetiredTargetRequest,
) MigrationRetiredResourceResult {
	if !validRetiredHTTPRequest(q) {
		return retiredHTTPFailure(q.Command.Seal.OperationID, ErrChannelAdmissionConflict)
	}

	hash, _ := admissionHash(q)

	return s.readRetiredHTTPFacts(ctx, q.Command.Seal.OperationID, hash, "")
}

func (s *ChannelAdmissionStore) CreateMigrationRetiredTargetFacts(
	ctx context.Context,
	q MigrationRetiredTargetRequest,
) MigrationRetiredResourceResult {
	if !validRetiredHTTPRequest(q) {
		return retiredHTTPFailure(q.Command.Seal.OperationID, ErrChannelAdmissionConflict)
	}

	r, e := s.PrepareRetiredTargetResource(ctx, q)
	if e != nil {
		return retiredHTTPFailure(q.Command.Seal.OperationID, e)
	}

	return s.readRetiredHTTPFacts(ctx, r.OperationID, r.RequestHash, r.CheckpointHash)
}

func (s *ChannelAdmissionStore) CancelMigrationRetiredTargetFacts(
	ctx context.Context,
	op, hash, checkpoint string,
) MigrationRetiredResourceResult {
	r, e := s.CancelRetiredTargetResource(ctx, op, hash, checkpoint)
	if e != nil {
		return retiredHTTPFailure(op, e)
	}

	return s.readRetiredHTTPFacts(ctx, r.OperationID, r.RequestHash, r.CheckpointHash)
}
