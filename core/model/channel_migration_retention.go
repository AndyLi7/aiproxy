package model

import (
	"context"
	"errors"
	"slices"

	"gorm.io/gorm"
)

const MigrationRetentionMaxRows = 4096

var errRetentionLimit = errors.New("retention source limit exceeded")

// Counts are observations, never closure capabilities. Null source counts mean
// the source is unavailable, not zero. Counts overlap and must not be added as
// distinct requests. No task payload, proof, credential or price is returned.
type MigrationRetentionResult struct {
	Version           int                        `json:"version"`
	Source            string                     `json:"source"`
	State             string                     `json:"state"`
	Reason            string                     `json:"reason"`
	OperationID       string                     `json:"operationId"`
	Epoch             string                     `json:"epoch"`
	Revision          string                     `json:"revision"`
	StorageGeneration string                     `json:"storageGeneration"`
	ObservedAtMs      string                     `json:"observedAtMs"`
	Identity          *ChannelIdentity           `json:"identity"`
	Receipt           *MigrationReadReceipt      `json:"receipt"`
	Cutover           string                     `json:"cutover"`
	Retention         string                     `json:"retention"`
	HistoryCoverage   string                     `json:"historyCoverage"`
	CacheSwitch       string                     `json:"cacheSwitch"`
	Authority         string                     `json:"authority"`
	Reasons           []string                   `json:"reasons"`
	Uses              *MigrationRetentionUses    `json:"uses"`
	Executions        []MigrationRetentionSource `json:"executions"`
	Native            *MigrationRetentionNative  `json:"native"`
}
type MigrationRetentionUses struct {
	ExactIdentityOpen           string `json:"exactIdentityOpen"`
	OtherHistoricalIdentityOpen string `json:"otherHistoricalIdentityOpen"`
	ClosedRecorded              string `json:"closedRecorded"`
	InvalidRecorded             string `json:"invalidRecorded"`
	OldEpochRecorded            string `json:"oldEpochRecorded"`
}
type MigrationRetentionCounts struct {
	Recorded              string `json:"recorded"`
	Active                string `json:"active"`
	UnfinishedHandoff     string `json:"unfinishedHandoff"`
	Returned              string `json:"returned"`
	ContinuationUnproven  string `json:"continuationUnproven"`
	InvalidBindingOrState string `json:"invalidBindingOrState"`
	HistoricalGeneration  string `json:"historicalGeneration"`
}
type MigrationRetentionSource struct {
	Kind   string                    `json:"kind"`
	State  string                    `json:"state"`
	Counts *MigrationRetentionCounts `json:"counts"`
}
type MigrationRetentionNative struct {
	Recorded                   string `json:"recorded"`
	Nonterminal                string `json:"nonterminal"`
	UnknownAcceptance          string `json:"unknownAcceptance"`
	TerminalAcceptanceUnproven string `json:"terminalAcceptanceUnproven"`
	OwnerPresent               string `json:"ownerPresent"`
	ExpiredOwnerPresent        string `json:"expiredOwnerPresent"`
	LocalBillingUnsettled      string `json:"localBillingUnsettled"`
	UnmatchedOriginalUse       string `json:"unmatchedOriginalUse"`
}

func retentionResult(op string) MigrationRetentionResult {
	return MigrationRetentionResult{
		Version:         1,
		Source:          "gateway-migration-retention-v1",
		State:           "unavailable",
		OperationID:     op,
		Cutover:         "unknown",
		Retention:       "unproven",
		HistoryCoverage: "unproven",
		CacheSwitch:     "not_observed_here",
		Authority:       "not_evaluated_by_diagnostic",
		Reasons:         []string{},
		Executions:      []MigrationRetentionSource{},
	}
}

func retentionReason(r *MigrationRetentionResult, reason string) {
	if slices.Contains(r.Reasons, reason) {
		return
	}

	r.Reasons = append(r.Reasons, reason)
}

// This method makes no online issuer call and never changes business state. It
// observes old epochs too: a fresh epoch or expired owner cannot erase history.
//
//nolint:gocyclo // A shared row budget and control snapshot govern all retained task, use, resource, and history classifications.
func (s *ChannelAdmissionStore) ReadMigrationRetention(
	ctx context.Context,
	op string,
) MigrationRetentionResult {
	out := retentionResult(op)
	if !ValidMigrationReadOperationID(op) {
		out.State = "conflict"
		out.Reason = "invalid_operation"
		return out
	}

	var cause error

	seen := s.migrationReadTransaction(
		ctx,
		func(tx *gorm.DB, c *ChannelAdmissionControl) (result MigrationReadResult, err error) {
			defer func() { cause = err }()

			original, e := readPreparedMigrationFactsTx(tx, c, op)
			if e != nil {
				return original, e
			}

			out.Epoch = original.Epoch
			out.Revision = original.Revision

			out.StorageGeneration = original.StorageGeneration
			if original.State != "ready" {
				if original.State == "absent" {
					if e := retentionNoOperationEffects(tx, op); e != nil {
						return original, e
					}
				}

				return original, nil
			}

			if original.Readback == nil {
				return original, ErrChannelAdmissionConflict
			}

			now, e := channelAdmissionTime(tx)
			if e != nil {
				return original, e
			}

			out.ObservedAtMs = migrationDecimal(now.UnixMilli())
			out.Identity = &original.Readback.From
			out.Receipt = &original.Readback.Receipt

			out.Cutover = "not_committed"
			if out.Receipt.State == "committed" {
				out.Cutover = "committed_current"
			}

			var uses []ChannelAdmissionUse
			if e = tx.Where("channel_id=?", out.Identity.ChannelID).
				Order("id").
				Limit(MigrationRetentionMaxRows + 1).
				Find(&uses).
				Error; e != nil {
				return original, e
			}

			budget := MigrationRetentionMaxRows
			if e = retentionBudget(&budget, len(uses)); e != nil {
				return original, e
			}

			byID := map[string]ChannelAdmissionUse{}
			ids := []string{}

			var exact, other, closed, invalid, old int64
			for _, u := range uses {
				bad := false
				byID[u.ID] = u

				ids = append(ids, u.ID)
				if u.Epoch != c.Epoch {
					old++
				}

				switch u.State {
				case "open":
					if u.IdentityHash == out.Identity.IdentityHash &&
						u.KeyDigest == out.Identity.KeyDigest {
						exact++
					} else {
						other++
					}

					if u.ClosedAtMs != 0 || u.ClosureHash != "" {
						bad = true
					}
				case "closed":
					closed++

					if u.ClosedAtMs <= 0 || !fullIdentityDigest(u.ClosureHash) {
						bad = true
					}
				default:
					bad = true
				}

				if !fullIdentityDigest(u.ID) || !fullIdentityDigest(u.IdentityHash) ||
					!fullIdentityDigest(u.KeyDigest) ||
					u.AdmittedAtMs <= 0 ||
					u.Epoch == "" ||
					!validAdmissionLane(u.Lane) ||
					!fullIdentityDigest(u.RequestHash) ||
					(u.ResourceKey != "" && !fullIdentityDigest(u.ResourceKey)) {
					bad = true
				}

				if bad {
					invalid++
				}
			}

			out.Uses = &MigrationRetentionUses{
				migrationDecimal(exact),
				migrationDecimal(other),
				migrationDecimal(closed),
				migrationDecimal(invalid),
				migrationDecimal(old),
			}
			if exact+other > 0 {
				out.Retention = "required"
				retentionReason(&out, "open_use_retained")
			}

			if old > 0 {
				retentionReason(&out, "history_generation_unproven")
			}

			if invalid > 0 {
				retentionReason(&out, "use_binding_or_state_unproven")
			}
			// Existing management policy refuses all persisted uses/barriers, including
			// closed reservations. This diagnostic does not remove that policy.
			if len(uses) > 0 || out.Receipt.State != "prepared" {
				retentionReason(&out, "existing_barrier_or_use_management_guard")
			}

			for _, spec := range retentionSources() {
				source, e := readRetentionSource(tx, c, spec, ids, byID, &budget)
				if e != nil {
					return original, e
				}

				out.Executions = append(out.Executions, source)
				if source.State != "observed" {
					retentionReason(&out, "execution_coverage_unavailable")
					continue
				}

				if source.Counts.Active != "0" {
					out.Retention = "required"
					retentionReason(&out, "known_execution_active")
				}

				if source.Counts.UnfinishedHandoff != "0" {
					out.Retention = "required"
					retentionReason(&out, "known_handoff_unfinished")
				}

				if source.Counts.ContinuationUnproven != "0" {
					retentionReason(&out, "continuation_unproven")
				}

				if source.Counts.InvalidBindingOrState != "0" {
					retentionReason(&out, "execution_binding_or_state_unproven")
				}

				if source.Counts.HistoricalGeneration != "0" {
					retentionReason(&out, "history_generation_unproven")
				}
			}

			out.Native, e = readRetentionNative(
				tx,
				out.Identity.ChannelID,
				uses,
				now.UnixMicro(),
				&budget,
			)
			if e != nil {
				return original, e
			}

			if out.Native == nil {
				retentionReason(&out, "native_task_coverage_unavailable")
			} else {
				if out.Native.Nonterminal != "0" {
					out.Retention = "required"
					retentionReason(&out, "native_task_not_terminal")
				}

				if out.Native.UnknownAcceptance != "0" {
					retentionReason(&out, "unknown_provider_acceptance")
				}

				if out.Native.TerminalAcceptanceUnproven != "0" {
					retentionReason(&out, "terminal_acceptance_unproven")
				}

				if out.Native.OwnerPresent != "0" {
					retentionReason(&out, "current_or_prior_owner_exit_unproven")
				}

				if out.Native.UnmatchedOriginalUse != "0" {
					retentionReason(&out, "native_task_binding_missing")
				}
				// LocalBillingUnsettled is informational only. It does not say a global
				// billing-key query depends on this physical inference channel.
			}

			retentionReason(&out, "history_coverage_unproven")
			retentionReason(&out, "switch_confirmation_not_observed")

			return original, nil
		},
	)
	out.State = seen.State

	out.Reason = seen.Reason
	if seen.State != "ready" {
		failed := retentionResult(op)
		failed.State = seen.State
		failed.Reason = seen.Reason
		failed.Epoch = seen.Epoch
		failed.Revision = seen.Revision

		failed.StorageGeneration = seen.StorageGeneration
		if errors.Is(cause, errRetentionLimit) {
			failed.Reason = "retention_source_limit_exceeded"
		}

		return failed
	}

	return out
}

// Count budgets apply across sources, never silently truncate a page.
func retentionBudget(left *int, n int) error {
	*left -= n
	if *left < 0 {
		return errRetentionLimit
	}

	return nil
}

// Each descriptor is server-owned SQL/schema metadata, never request input.
type retentionSourceSpec struct {
	kind, table       string
	model             any
	protocol, version int
	parent            string
	handoff, measured bool
}

func retentionSources() []retentionSourceSpec {
	return []retentionSourceSpec{
		{
			"relay",
			"channel_admission_executions",
			&ChannelAdmissionExecution{},
			1,
			1,
			"",
			false,
			false,
		},
		{
			"native",
			"channel_admission_task_executions",
			&ChannelAdmissionTaskExecution{},
			2,
			NativeExecutionProtocolVersion,
			"",
			false,
			false,
		},
		{
			"native_dispatch",
			"channel_admission_dispatch_handoffs",
			&ChannelAdmissionDispatchHandoff{},
			3,
			1,
			"channel_admission_task_executions",
			true,
			false,
		},
		{
			"native_recovery",
			"channel_admission_recovery_handoffs",
			&ChannelAdmissionRecoveryHandoff{},
			4,
			1,
			"channel_admission_task_executions",
			true,
			false,
		},
		{
			"image_recovery",
			"channel_admission_image_recovery_handoffs",
			&ChannelAdmissionImageRecoveryHandoff{},
			6,
			1,
			"",
			true,
			false,
		},
		{
			"image_poll",
			"channel_admission_image_poll_handoffs",
			&ChannelAdmissionImagePollHandoff{},
			7,
			1,
			"",
			true,
			false,
		},
		{
			"image_producer",
			"channel_admission_image_producers",
			&ChannelAdmissionImageProducer{},
			8,
			1,
			"",
			false,
			false,
		},
		{
			"measured_producer",
			"channel_admission_measured_producers",
			&ChannelAdmissionMeasuredProducer{},
			9,
			1,
			"",
			false,
			true,
		},
		{
			"measured_claim",
			"channel_admission_measured_claims",
			&ChannelAdmissionMeasuredClaim{},
			9,
			1,
			"channel_admission_measured_producers",
			true,
			true,
		},
	}
}

func retentionTable(tx *gorm.DB, table string) (bool, error) {
	var (
		n int64
		e error
	)
	switch tx.Name() {
	case "sqlite":
		e = tx.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).
			Scan(&n).
			Error
	case "postgres":
		e = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=?", table).
			Scan(&n).
			Error
	default:
		return false, ErrChannelAdmissionUnavailable
	}

	return n == 1, e
}

func retentionColumns(tx *gorm.DB, m any) error {
	st := &gorm.Statement{DB: tx}
	if e := st.Parse(m); e != nil {
		return e
	}

	rows, e := tx.Model(m).Select(st.Schema.DBNames).Limit(0).Rows()
	if e != nil {
		return e
	}

	e = rows.Err()
	//nolint:sqlclosecheck // The LIMIT 0 schema probe must close before the next query on this transaction.
	closeErr := rows.Close()

	if e != nil {
		return e
	}

	return closeErr
}

type retentionExecutionRow struct {
	ID, UseID, UseHash, State, Epoch, StorageGeneration, ContinuationState, ExecutionState, ParentHash, ExpectedParentHash string
	Version                                                                                                                int
	StartedAtMs, ReturnedAtMs                                                                                              int64
}

//nolint:gocyclo // Each bounded history source checks its parent, use, generation, and lifecycle facts before classifying retention.
func readRetentionSource(
	tx *gorm.DB,
	c *ChannelAdmissionControl,
	spec retentionSourceSpec,
	ids []string,
	uses map[string]ChannelAdmissionUse,
	budget *int,
) (MigrationRetentionSource, error) {
	out := MigrationRetentionSource{Kind: spec.kind, State: "unavailable"}
	for _, table := range []string{spec.table, "channel_admission_execution_protocols"} {
		ok, e := retentionTable(tx, table)
		if e != nil {
			return out, e
		}

		if !ok {
			return out, nil
		}
	}

	if e := retentionColumns(tx, spec.model); e != nil {
		return out, e
	}

	var protocol ChannelAdmissionExecutionProtocol

	e := tx.Where("id=?", spec.protocol).First(&protocol).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return out, nil
	}

	if e != nil {
		return out, e
	}

	if protocol.Version != spec.version {
		out.State = "invalid_protocol"
		return out, nil
	}

	columns := "r.id, r.use_id, r.use_hash, r.state, r.epoch, r.storage_generation, r.continuation_state, r.version, r.started_at_ms, r.returned_at_ms"
	scope := "r.use_id"

	q := tx.Table(spec.table + " AS r")
	if spec.handoff {
		columns = "r.id, r.use_id, r.use_hash, r.state, r.epoch, r.storage_generation, r.continuation_state, r.version, r.offered_at_ms AS started_at_ms, r.exited_at_ms AS returned_at_ms,r.execution_state"
	}

	if spec.measured && !spec.handoff {
		columns = "r.id,r.use_id,r.use_hash,r.state,'' AS epoch,r.storage_generation,'unproven' AS continuation_state,1 AS version,r.started_at_ms,r.returned_at_ms"
	}

	if spec.parent != "" {
		ok, e := retentionTable(tx, spec.parent)
		if e != nil {
			return out, e
		}

		if !ok {
			return out, nil
		}

		if spec.measured {
			if e = retentionColumns(tx, &ChannelAdmissionMeasuredProducer{}); e != nil {
				return out, e
			}

			q = q.Joins("JOIN channel_admission_measured_producers AS p ON p.id=r.source_id")
			columns = "r.id,p.use_id,p.use_hash,r.state,r.epoch,r.storage_generation,'unproven' AS continuation_state,1 AS version,r.offered_at_ms AS started_at_ms,r.exited_at_ms AS returned_at_ms"
		} else {
			if e = retentionColumns(tx, &ChannelAdmissionTaskExecution{}); e != nil {
				return out, e
			}

			q = q.Joins("JOIN channel_admission_task_executions AS p ON p.id=r.id")
			columns = "r.id,p.use_id,p.use_hash,r.state,p.epoch,p.storage_generation,p.continuation_state,r.version,r.offered_at_ms AS started_at_ms,r.exited_at_ms AS returned_at_ms,r.execution_hash AS parent_hash,p.authorization_hash AS expected_parent_hash,p.state AS execution_state"
		}

		scope = "p.use_id"
	}

	var rows []retentionExecutionRow
	if len(ids) > 0 {
		if e = q.Select(columns).
			Where(scope+" IN ?", ids).
			Order("r.id").
			Limit(*budget + 1).
			Scan(&rows).
			Error; e != nil {
			return out, e
		}
	}

	if e = retentionBudget(budget, len(rows)); e != nil {
		return out, e
	}

	var active, unfinished, returned, unproven, invalid, old int64
	for _, r := range rows {
		u, found := uses[r.UseID]

		bad := !found || r.UseHash != executionUseHash(&u) || r.Version != spec.version ||
			r.StartedAtMs <= 0 ||
			!fullIdentityDigest(r.ID)
		if spec.parent != "" && !spec.measured && r.ParentHash != r.ExpectedParentHash {
			bad = true
		}

		if r.StorageGeneration != c.StorageGeneration || (r.Epoch != "" && r.Epoch != c.Epoch) {
			old++
		}

		if spec.handoff && !spec.measured && r.ExecutionState != "active" &&
			r.ExecutionState != "returned" &&
			r.ExecutionState != "error_returned" &&
			r.ExecutionState != "async_returned" {
			bad = true
		}

		if r.ExecutionState == "active" && r.State != "active" {
			active++
		}

		if r.State == "exited" && r.ExecutionState == "active" {
			bad = true
		}

		switch r.State {
		case "active":
			active++

			if spec.handoff || r.ReturnedAtMs != 0 {
				bad = true
			}
		case "offered", "accepted":
			unfinished++

			if !spec.handoff || r.ReturnedAtMs != 0 {
				bad = true
			}
		case "returned", "error_returned", "async_returned":
			returned++

			if spec.handoff || r.ReturnedAtMs < r.StartedAtMs {
				bad = true
			}
		case "exited":
			returned++

			if !spec.handoff || r.ReturnedAtMs < r.StartedAtMs {
				bad = true
			}
		default:
			bad = true
		}
		// Existing rows have no supported positive continuation-closure evidence.
		// Future/unknown values do not silently become proven.
		unproven++

		if r.ContinuationState != "unproven" {
			bad = true
		}

		if bad {
			invalid++
		}
	}

	out.State = "observed"
	out.Counts = &MigrationRetentionCounts{
		migrationDecimal(int64(len(rows))),
		migrationDecimal(active),
		migrationDecimal(unfinished),
		migrationDecimal(returned),
		migrationDecimal(unproven),
		migrationDecimal(invalid),
		migrationDecimal(old),
	}

	return out, nil
}

//nolint:gocyclo // Native retention distinguishes terminal tasks, active leases, pending usage, and operation links within one bounded scan.
func readRetentionNative(
	tx *gorm.DB,
	channel int,
	uses []ChannelAdmissionUse,
	nowMicros int64,
	budget *int,
) (*MigrationRetentionNative, error) {
	ok, e := retentionTable(tx, "native_tasks")
	if e != nil || !ok {
		return nil, e
	}

	if e = retentionColumns(tx, &NativeTask{}); e != nil {
		return nil, e
	}

	var tasks []NativeTask

	e = tx.Select("id,group_id,token_id,fingerprint,status,upstream_id,key_fingerprint,billing_settled,recovery_owner,recovery_until,dispatch_owner,dispatch_until").
		Where("channel_id=?", channel).
		Order("id").
		Limit(*budget + 1).
		Find(&tasks).
		Error
	if e != nil {
		return nil, e
	}

	if e = retentionBudget(budget, len(tasks)); e != nil {
		return nil, e
	}

	matched := map[string]bool{}

	openByResource := map[string][]ChannelAdmissionUse{}
	for _, u := range uses {
		if u.Lane == "native" && u.State != "closed" {
			openByResource[u.ResourceKey] = append(openByResource[u.ResourceKey], u)
		}
	}

	var nonterminal, unknown, terminalUnknown, owner, expired, billing int64
	for _, t := range tasks {
		resource := nativeAdmissionResource(t.ID, t.GroupID, t.TokenID)

		candidates := openByResource[resource]
		for _, u := range candidates {
			if u.RequestHash == t.Fingerprint && u.KeyDigest == t.KeyFingerprint {
				matched[u.ID] = true
			}
		}

		if t.Status != "completed" && t.Status != "failed" {
			nonterminal++
		}

		if t.Status == "submitting" || t.Status == "submission_unknown" {
			unknown++
		}

		if t.Status == "failed" && t.UpstreamID == "" && len(candidates) > 0 {
			terminalUnknown++
		}

		if t.RecoveryOwner != "" || t.DispatchOwner != "" {
			owner++
		}

		if (t.RecoveryOwner != "" && t.RecoveryUntil <= nowMicros/1000000) ||
			(t.DispatchOwner != "" && t.DispatchUntil != nil && t.DispatchUntil.UnixMicro() <= nowMicros) {
			expired++
		}

		if !t.BillingSettled {
			billing++
		}
	}

	var missing int64
	for _, u := range uses {
		if u.Lane == "native" && u.State != "closed" && !matched[u.ID] {
			missing++
		}
	}

	return &MigrationRetentionNative{
		migrationDecimal(int64(len(tasks))),
		migrationDecimal(nonterminal),
		migrationDecimal(unknown),
		migrationDecimal(terminalUnknown),
		migrationDecimal(owner),
		migrationDecimal(expired),
		migrationDecimal(billing),
		migrationDecimal(missing),
	}, nil
}

// Missing Prepared is not absence when an owned resource/run survives. Optional
// installations must be complete and understood before an absent observation.
func retentionNoOperationEffects(tx *gorm.DB, op string) error {
	for _, s := range []struct {
		table, protocol string
		model           any
		version         int
	}{
		{"channel_migration_shadow_resources", "channel_migration_shadow_protocols", &ChannelMigrationShadowResource{}, 2},
		{"channel_migration_probe_runs", "channel_migration_probe_runtime_protocols", &ChannelMigrationProbeRun{}, 1},
		{"channel_migration_retired_target_resources", "", &ChannelMigrationRetiredTargetResource{}, 1},
	} {
		present, e := retentionTable(tx, s.table)
		if e != nil {
			return e
		}

		if s.protocol != "" {
			protocol, e := retentionTable(tx, s.protocol)
			if e != nil {
				return e
			}

			if present != protocol {
				return ErrChannelAdmissionUnavailable
			}

			if present {
				var row struct{ ID, Version int }
				if e = tx.Table(s.protocol).
					Where("id=1").
					Take(&row).
					Error; e != nil ||
					row.Version != s.version {
					return ErrChannelAdmissionUnavailable
				}
			}
		}

		if !present {
			continue
		}

		if e = retentionColumns(tx, s.model); e != nil {
			return e
		}

		var count int64
		if e = tx.Table(s.table).Where("operation_id=?", op).Count(&count).Error; e != nil {
			return e
		}

		if count != 0 {
			return ErrChannelAdmissionConflict
		}
	}

	return nil
}
