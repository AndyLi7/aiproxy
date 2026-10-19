package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode"

	"github.com/labring/aiproxy/core/common/nativeresult"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// This reader only relates already-persisted facts. It never calls a wallet,
// provider, issuer or Finalize, and it never reauthorizes a cancelled operation.
type MigrationProbeBillingContextRequest struct {
	BillingOperationID string `json:"billingOperationId"`
	TaskID             string `json:"taskId"`
	GroupID            string `json:"groupId"`
}
type MigrationProbeBillingContext struct {
	MigrationOperationID string                       `json:"migrationOperationId"`
	AuthorizationHash    string                       `json:"authorizationHash"`
	CommandHash          string                       `json:"commandHash"`
	ResourceRequestHash  string                       `json:"resourceRequestHash"`
	CheckpointHash       string                       `json:"checkpointHash"`
	SpecHash             string                       `json:"specHash"`
	UseID                string                       `json:"useId"`
	TaskBindingHash      string                       `json:"taskBindingHash"`
	From                 ChannelIdentity              `json:"from"`
	To                   ChannelIdentity              `json:"to"`
	FromCredentialID     string                       `json:"fromCredentialId"`
	ToCredentialID       string                       `json:"toCredentialId"`
	Binding              MigrationProbeBillingRequest `json:"binding"`
	ResourceState        string                       `json:"resourceState"`
	TaskStatus           string                       `json:"taskStatus"`
	BillingSettled       bool                         `json:"billingSettled"`
	TargetResourceKind   string                       `json:"targetResourceKind,omitempty"`
	RevertOf             string                       `json:"revertOf,omitempty"`
}
type MigrationProbeBillingContextResult struct {
	Version           int                           `json:"version"`
	Source            string                        `json:"source"`
	State             string                        `json:"state"`
	Reason            string                        `json:"reason"`
	Epoch             string                        `json:"epoch"`
	StorageGeneration string                        `json:"storageGeneration"`
	Revision          string                        `json:"revision"`
	Context           *MigrationProbeBillingContext `json:"context"`
}

func probeBillingContextResult(state, reason string) MigrationProbeBillingContextResult {
	return MigrationProbeBillingContextResult{
		Version: 1,
		Source:  "gateway-migration-probe-billing-context-v1",
		State:   state,
		Reason:  reason,
	}
}

func boundedProbeContextID(s string, maximum int) bool {
	return s != "" && len(s) <= maximum &&
		!strings.ContainsFunc(s, unicode.IsControl)
}

func ValidMigrationProbeBillingContextRequest(r MigrationProbeBillingContextRequest) bool {
	return boundedProbeContextID(r.BillingOperationID, 256) &&
		boundedProbeContextID(r.TaskID, 128) &&
		boundedProbeContextID(r.GroupID, 64)
}

func probeBillingTaskMatches(t *NativeTask, spec MigrationProbeSpec) bool {
	contract, e := nativeresult.CompileTaskContract(spec.Contract)
	if e != nil {
		return false
	}

	input, e := contract.ValidateRequest(spec.Input)
	if e != nil {
		return false
	}

	nativeInput, e := contract.PrepareUpstreamInput(input)
	if e != nil {
		return false
	}

	var frozen nativeresult.TaskContract
	if json.Unmarshal(spec.Contract, &frozen) != nil {
		return false
	}

	return t.ID == spec.TaskID && t.GroupID == spec.GroupID && t.TokenID == spec.TokenID &&
		t.Model == frozen.Model &&
		t.Fingerprint == shadowDigest(string(spec.Input)) &&
		t.OutputSchema == string(frozen.OutputSchema) &&
		t.FrozenContract == string(spec.Contract) &&
		t.NativeInput == string(nativeInput) &&
		t.ChannelID == spec.ChannelID &&
		t.Endpoint == spec.Endpoint &&
		t.CredentialScope == spec.CredentialScope &&
		t.PrepaymentQuoteJSON == spec.QuoteJSON &&
		t.BillingOperationID == "native:"+spec.TaskID &&
		t.DeliveryBase == spec.DeliveryBase &&
		t.KeyFingerprint == spec.KeyDigest
}

//nolint:gocyclo // The billing context is tied to the exact operation, resource, probe run, native task, and scope before returning any facts.
func probeBillingContextTx(
	tx *gorm.DB,
	c *ChannelAdmissionControl,
	input MigrationProbeBillingContextRequest,
) (MigrationProbeBillingContextResult, error) {
	if e := probeRuntimeStorageReady(tx); e != nil {
		return MigrationProbeBillingContextResult{}, e
	}

	if e := shadowStorageReady(tx); e != nil {
		return MigrationProbeBillingContextResult{}, e
	}
	// Complete task schema is required even for a non-association answer.
	st := &gorm.Statement{DB: tx}
	if e := st.Parse(&NativeTask{}); e != nil {
		return MigrationProbeBillingContextResult{}, e
	}

	columns := make([]string, 0, len(st.Schema.DBNames))
	for _, name := range st.Schema.DBNames {
		columns = append(columns, st.Quote(clause.Column{Table: st.Schema.Table, Name: name}))
	}

	rows, e := tx.Model(&NativeTask{}).Select(strings.Join(columns, ",")).Limit(0).Rows()
	if e != nil {
		return MigrationProbeBillingContextResult{}, ErrChannelAdmissionUnavailable
	}

	//nolint:sqlclosecheck // The LIMIT 0 probe must close before the next query on this transaction.
	re, ce := rows.Err(), rows.Close()
	if re != nil || ce != nil {
		return MigrationProbeBillingContextResult{}, ErrChannelAdmissionUnavailable
	}

	var candidates []ChannelMigrationProbeRun
	// Do not trust a missing unique index, and never select a run by caller op/name.
	if e := tx.Where("task_id=?", input.TaskID).Limit(2).Find(&candidates).Error; e != nil {
		return MigrationProbeBillingContextResult{}, e
	}

	if len(candidates) > 1 {
		return MigrationProbeBillingContextResult{}, ErrChannelAdmissionConflict
	}

	if len(candidates) == 0 {
		var n int64
		if e := tx.Model(&ChannelAdmissionUse{}).
			Where("id=?", probeOperationUseID(input.TaskID)).
			Count(&n).
			Error; e != nil {
			return MigrationProbeBillingContextResult{}, e
		}

		if n > 0 || strings.HasPrefix(input.TaskID, "probe_") {
			return probeBillingContextResult("unknown", "probe_history_missing"), nil
		}
		// This is current complete-storage non-association, not a statement about
		// lost history, an independent recovery watermark or permission to regenerate.
		return probeBillingContextResult("not_probe", "no_probe_in_current_storage"), nil
	}

	run, a, spec, e := loadProbeRun(tx, candidates[0].OperationID, true)
	if e != nil {
		return MigrationProbeBillingContextResult{}, e
	}

	if len(run.AuthorizationJSON) > 384*1024 || len(run.SpecJSON) > 1024*1024 ||
		!validProbeCommand(a.Command) ||
		!ValidMigrationReadOperationID(run.OperationID) {
		return MigrationProbeBillingContextResult{}, ErrChannelAdmissionConflict
	}

	lifecycle, e := probeBillingOriginalResourceTx(tx, c, run, a, spec)
	if e != nil {
		return MigrationProbeBillingContextResult{}, e
	}

	if run.StorageGeneration != c.StorageGeneration || a.Command.Seal.Epoch != c.Epoch ||
		a.GroupID != spec.GroupID ||
		a.TokenID != spec.TokenID ||
		string(a.Input) != string(spec.Input) ||
		spec.ChannelID != a.Command.Seal.To.ChannelID ||
		spec.KeyDigest != a.Command.Seal.To.KeyDigest ||
		spec.IdentityHash != a.Command.Seal.To.IdentityHash ||
		spec.CredentialScope != a.Command.ToCredentialID ||
		input.GroupID != spec.GroupID ||
		input.BillingOperationID != "native:"+spec.TaskID {
		return MigrationProbeBillingContextResult{}, ErrChannelAdmissionConflict
	}

	if run.UseID == "" {
		return probeBillingContextResult("pending", "task_not_bound"), nil
	}

	var t NativeTask
	if e = tx.First(&t, "id=?", run.TaskID).Error; e != nil {
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return probeBillingContextResult("unknown", "bound_task_missing"), nil
		}
		return MigrationProbeBillingContextResult{}, e
	}

	if !probeBillingTaskMatches(&t, spec) || nativeExecutionTaskHash(&t) != run.TaskHash ||
		t.BillingOperationID != input.BillingOperationID {
		return MigrationProbeBillingContextResult{}, ErrChannelAdmissionConflict
	}

	var use ChannelAdmissionUse
	if e = tx.First(&use, "id=?", run.UseID).Error; e != nil {
		return MigrationProbeBillingContextResult{}, e
	}

	if use.ResourceKey != nativeAdmissionResource(t.ID, t.GroupID, t.TokenID) ||
		use.RequestHash != t.Fingerprint ||
		use.ChannelID != spec.ChannelID ||
		use.IdentityHash != spec.IdentityHash ||
		use.KeyDigest != spec.KeyDigest ||
		use.Lane != "native" ||
		use.Epoch != a.Command.Seal.Epoch ||
		use.AdmittedAtMs <= 0 ||
		use.AdmittedAtMs != run.BoundAtMs ||
		use.State != "open" {
		return MigrationProbeBillingContextResult{}, ErrChannelAdmissionConflict
	}

	if !boundedProbeContextID(t.UpstreamID, 256) {
		return probeBillingContextResult("pending", "upstream_not_accepted"), nil
	}

	quote, e := ParseImagePrepaymentQuote(spec.QuoteJSON)
	if e != nil {
		return MigrationProbeBillingContextResult{}, ErrChannelAdmissionConflict
	}

	route := quote.Route(spec.ChannelID, spec.Endpoint)
	if route == nil || route.Provider != "fal" || route.CredentialScope != spec.CredentialScope ||
		(route.QuantityMetric != "request" && route.QuantityMetric != "bounded_request") ||
		quote.PrepaidMicros > a.MaxPrepaidMicros {
		return MigrationProbeBillingContextResult{}, ErrChannelAdmissionConflict
	}

	out := probeBillingContextResult("ready", "")
	out.Context = &MigrationProbeBillingContext{
		run.OperationID,
		run.AuthorizationHash,
		run.CommandHash,
		run.ResourceRequestHash,
		run.CheckpointHash,
		run.SpecHash,
		run.UseID,
		run.TaskHash,
		a.Command.Seal.From,
		a.Command.Seal.To,
		a.Command.FromCredentialID,
		a.Command.ToCredentialID,
		probeBillingRequest(&t, a),
		lifecycle,
		t.Status,
		t.BillingSettled,
		a.Command.TargetResourceKind,
		a.Command.Seal.RevertOf,
	}

	return out, nil
}

func (s *ChannelAdmissionStore) ReadMigrationProbeBillingContext(
	ctx context.Context,
	input MigrationProbeBillingContextRequest,
) MigrationProbeBillingContextResult {
	if !ValidMigrationProbeBillingContextRequest(input) {
		return probeBillingContextResult("conflict", "invalid_billing_context_request")
	}

	var out MigrationProbeBillingContextResult

	r := s.migrationReadTransaction(
		ctx,
		func(tx *gorm.DB, c *ChannelAdmissionControl) (MigrationReadResult, error) {
			var e error

			out, e = probeBillingContextTx(tx, c, input)
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
		state := "unavailable"
		if r.State == "conflict" {
			state = "conflict"
		}

		return probeBillingContextResult(state, "billing_context_"+state)
	}

	return out
}
