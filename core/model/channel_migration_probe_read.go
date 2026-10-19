package model

import (
	"context"
	"encoding/json"
	"errors"

	"gorm.io/gorm"
)

// This observation never declares a probe successful from task status or wallet
// settlement alone. A separate verified billing provenance is mandatory.
type MigrationProbeResult struct {
	Version           int                        `json:"version"`
	Source            string                     `json:"source"`
	State             string                     `json:"state"`
	Reason            string                     `json:"reason"`
	OperationID       string                     `json:"operationId"`
	Epoch             string                     `json:"epoch"`
	StorageGeneration string                     `json:"storageGeneration"`
	Revision          string                     `json:"revision"`
	Observation       *MigrationProbeObservation `json:"observation"`
}
type MigrationProbeObservation struct {
	AuthorizationHash   string `json:"authorizationHash"`
	CommandHash         string `json:"commandHash"`
	ResourceRequestHash string `json:"resourceRequestHash"`
	CheckpointHash      string `json:"checkpointHash"`
	TaskID              string `json:"taskId"`
	TaskStatus          string `json:"taskStatus"`
	SpecHash            string `json:"specHash"`
	UseID               string `json:"useId"`
	CreatedAtMs         string `json:"createdAtMs"`
	BoundAtMs           string `json:"boundAtMs"`
}

func probeResult(op, state, reason string) MigrationProbeResult {
	return MigrationProbeResult{
		Version:     1,
		Source:      "gateway-migration-probe-v1",
		OperationID: op,
		State:       state,
		Reason:      reason,
	}
}

func probeResultFailure(op string, e error) MigrationProbeResult {
	if errors.Is(e, ErrChannelAdmissionConflict) || errors.Is(e, gorm.ErrRecordNotFound) {
		return probeResult(op, "conflict", "probe_binding_conflict")
	}
	return probeResult(op, "unavailable", "probe_storage_or_runtime_unavailable")
}

//nolint:gocyclo // Observation must classify resource, run, task, and billing facts from one generation-consistent snapshot.
func probeObservationTx(
	tx *gorm.DB,
	c *ChannelAdmissionControl,
	op string,
) (MigrationProbeResult, error) {
	if e := probeRuntimeStorageReady(tx); e != nil {
		return MigrationProbeResult{}, e
	}

	resource, e := migrationTargetByOperation(tx, op, true)
	if e != nil {
		return MigrationProbeResult{}, e
	}

	run, a, spec, e := loadProbeRun(tx, op, true)
	if errors.Is(e, gorm.ErrRecordNotFound) {
		if resource != nil {
			taskID := probeOperationTaskID(resource.StorageGeneration, op, resource.CheckpointHash)

			var tasks, uses int64
			if e = tx.Model(&NativeTask{}).Where("id=?", taskID).Count(&tasks).Error; e != nil {
				return MigrationProbeResult{}, e
			}

			if e = tx.Model(&ChannelAdmissionUse{}).
				Where("id=?", probeOperationUseID(taskID)).
				Count(&uses).
				Error; e != nil {
				return MigrationProbeResult{}, e
			}

			if tasks != 0 || uses != 0 {
				return probeResult(op, "unknown", "probe_history_missing"), nil
			}
		}
		// Absence only describes the current complete database, not a recovery watermark.
		if e = shadowNoOperationEffects(tx, op); e != nil {
			return MigrationProbeResult{}, e
		}

		return probeResult(op, "absent", ""), nil
	}

	if e != nil {
		return MigrationProbeResult{}, e
	}

	if resource == nil ||
		(resource.Epoch != c.Epoch || resource.StorageGeneration != c.StorageGeneration || resource.Revision > c.Revision || resource.Kind != a.Command.TargetResourceKind) ||
		run.ResourceRequestHash != resource.RequestHash ||
		run.CheckpointHash != resource.CheckpointHash ||
		a.Command.Seal.From.ChannelID != resource.FromChannelID ||
		a.Command.Seal.To != resource.Target ||
		run.StorageGeneration != c.StorageGeneration ||
		a.Command.Seal.Epoch != c.Epoch {
		return MigrationProbeResult{}, ErrChannelAdmissionConflict
	}

	result := probeResult(op, "pending", "task_not_bound")
	result.Epoch = c.Epoch
	result.StorageGeneration = c.StorageGeneration
	result.Revision = migrationDecimal(c.Revision)

	result.Observation = &MigrationProbeObservation{
		run.AuthorizationHash,
		run.CommandHash,
		run.ResourceRequestHash,
		run.CheckpointHash,
		run.TaskID,
		"",
		run.SpecHash,
		run.UseID,
		migrationDecimal(run.CreatedAtMs),
		migrationDecimal(run.BoundAtMs),
	}
	if resource.LifecycleState == "cancelled" {
		result.State = "cancelled"
		result.Reason = "operation_cancelled"
		return result, nil
	}

	if run.UseID == "" {
		return result, nil
	}

	var task NativeTask

	e = tx.First(
		&task,
		"id=? AND group_id=? AND token_id=?",
		run.TaskID,
		spec.GroupID,
		spec.TokenID,
	).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		result.State = "unknown"
		result.Reason = "bound_task_missing"
		return result, nil
	}

	if e != nil {
		return MigrationProbeResult{}, e
	}

	var use ChannelAdmissionUse
	if tx.First(&use, "id=?", run.UseID).Error != nil ||
		use.ResourceKey != nativeAdmissionResource(task.ID, task.GroupID, task.TokenID) ||
		use.RequestHash != task.Fingerprint ||
		use.ChannelID != spec.ChannelID ||
		use.IdentityHash != spec.IdentityHash ||
		use.KeyDigest != spec.KeyDigest ||
		use.Lane != "native" ||
		nativeExecutionTaskHash(&task) != run.TaskHash {
		return MigrationProbeResult{}, ErrChannelAdmissionConflict
	}

	if use.Epoch != a.Command.Seal.Epoch || use.AdmittedAtMs <= 0 {
		return MigrationProbeResult{}, ErrChannelAdmissionConflict
	}

	if use.State != "open" {
		result.State = "unknown"
		result.Reason = "probe_use_not_open"
		return result, nil
	}

	result.Observation.TaskStatus = task.Status
	switch task.Status {
	case "failed":
		result.State = "failed"
		result.Reason = "task_failed"
	case "submission_unknown":
		result.State = "unknown"
		result.Reason = "submission_outcome_unknown"
	case "completed":
		result.Reason = "billing_provenance_pending"
	default:
		result.Reason = "task_pending"
	}

	return result, nil
}

func (s *ChannelAdmissionStore) ReadMigrationProbe(
	ctx context.Context,
	op string,
) MigrationProbeResult {
	if !ValidMigrationReadOperationID(op) {
		return probeResultFailure(op, ErrChannelAdmissionConflict)
	}

	var out MigrationProbeResult

	r := s.migrationReadTransaction(
		ctx,
		func(tx *gorm.DB, c *ChannelAdmissionControl) (MigrationReadResult, error) {
			var e error

			out, e = probeObservationTx(tx, c, op)
			if e != nil {
				return MigrationReadResult{}, e
			}

			out.Epoch = c.Epoch
			out.StorageGeneration = c.StorageGeneration
			out.Revision = migrationDecimal(c.Revision)

			return migrationReadState("ready", ""), nil
		},
	)
	if r.State != "ready" {
		return probeResult(op, r.State, r.Reason)
	}

	return out
}

// MigrationProbeRuntimeStore resolves the actual server bootstrap configuration.
// It has no request-selected issuer, executor, manifest, or test fallback.
func MigrationProbeRuntimeStore(
	db, logs *gorm.DB,
	features []string,
) (*ChannelAdmissionStore, error) {
	if db == nil || db != logs {
		return nil, ErrChannelAdmissionUnavailable
	}

	s, e := channelAdmissionRuntimeStore(db)
	if e != nil || s == nil {
		return nil, ErrChannelAdmissionUnavailable
	}

	s.MigrationFeatures = append([]string(nil), features...)

	return s, nil
}

// Probe authorization is the original request, not a mutable task-level guess.
func (s *ChannelAdmissionStore) MigrationProbeAuthorization(
	ctx context.Context,
	op string,
) (out MigrationProbeAuthorization, err error) {
	r := s.migrationReadTransaction(
		ctx,
		func(tx *gorm.DB, c *ChannelAdmissionControl) (MigrationReadResult, error) {
			if _, e := probeObservationTx(tx, c, op); e != nil {
				return MigrationReadResult{}, e
			}

			_, a, _, e := loadProbeRun(tx, op, true)
			if e != nil {
				return MigrationReadResult{}, e
			}

			encoded, _ := json.Marshal(a)
			if json.Unmarshal(encoded, &out) != nil {
				return MigrationReadResult{}, ErrChannelAdmissionConflict
			}

			return migrationReadState("ready", ""), nil
		},
	)
	if r.State != "ready" {
		err = ErrChannelAdmissionUnavailable
	}

	return
}
