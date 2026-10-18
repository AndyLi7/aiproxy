package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrMigrationProbePending = errors.New("migration probe evidence pending")
var ErrMigrationProbeMissingProvenance = errors.New("migration probe billing provenance unavailable")

// The app coordinator authenticates the original probe context and settled
// monetary facts, then records a separate current workspace/bill attribution.
// It never substitutes current credentials for missing original probe identity
// or claims historical continuous workspace membership.
type MigrationProbeBillingRequest struct {
	MigrationOperationID string `json:"migrationOperationId"`
	FromKeyDigest        string `json:"fromKeyDigest"`
	BillingOperationID   string `json:"billingOperationId"`
	TaskID               string `json:"taskId"`
	UpstreamTaskID       string `json:"upstreamTaskId"`
	GroupID              string `json:"groupId"`
	Provider             string `json:"provider"`
	Endpoint             string `json:"endpoint"`
	CredentialScope      string `json:"credentialScope"`
	KeyDigest            string `json:"keyDigest"`
	QuoteHash            string `json:"quoteHash"`
}
type MigrationProbeBillingEvidence struct {
	Binding                       MigrationProbeBillingRequest `json:"binding"`
	EvidenceID                    string                       `json:"evidenceId"`
	ProvenanceHash                string                       `json:"provenanceHash"`
	AccountMembershipEvidenceHash string                       `json:"accountMembershipEvidenceHash"`
	AccountMembershipEvidenceKind string                       `json:"accountMembershipEvidenceKind"`
	Currency                      string                       `json:"currency"`
	CostMicros                    int64                        `json:"costMicros"`
	CustomerDueMicros             int64                        `json:"customerDueMicros"`
	ChargedMicros                 int64                        `json:"chargedMicros"`
	SettledAtMs                   int64                        `json:"settledAtMs"`
}
type MigrationProbeBillingReader func(context.Context, MigrationProbeBillingRequest) (MigrationProbeBillingEvidence, error)

func probeTaskFactHash(t *NativeTask) string {
	h, _ := admissionHash([]any{nativeExecutionTaskHash(t), t.Status, t.UpstreamID, t.NativeOutput, t.DeliveredOutput, t.ArtifactManifest, t.BillingSettled, t.BillingReceiptJSON})
	return h
}
func probeDeliveryAndSettlement(t *NativeTask) error {
	if t.Status != "completed" || t.UpstreamID == "" || !t.BillingSettled {
		return ErrMigrationProbePending
	}
	compiled, e := nativeresult.CompileTaskContract([]byte(t.FrozenContract))
	if e != nil {
		return ErrChannelAdmissionConflict
	}
	if _, e = compiled.ValidateOutput([]byte(t.NativeOutput)); e != nil {
		return ErrChannelAdmissionConflict
	}
	if _, e = compiled.ValidateOutput([]byte(t.DeliveredOutput)); e != nil {
		return ErrChannelAdmissionConflict
	}
	var contract nativeresult.TaskContract
	if json.Unmarshal([]byte(t.FrozenContract), &contract) != nil {
		return ErrChannelAdmissionConflict
	}
	original, e := nativeresult.PlanArtifacts([]byte(t.NativeOutput), contract.Artifacts)
	if e != nil {
		return ErrChannelAdmissionConflict
	}
	delivered, e := nativeresult.PlanDeliveredArtifacts([]byte(t.DeliveredOutput), contract.Artifacts, t.DeliveryBase)
	if e != nil || len(original) != len(delivered) {
		return ErrChannelAdmissionConflict
	}
	entries := map[int]ownedartifact.Receipt{}
	if t.ArtifactManifest != "" && json.Unmarshal([]byte(t.ArtifactManifest), &entries) != nil {
		return ErrChannelAdmissionConflict
	}
	if len(entries) != len(original) {
		return ErrChannelAdmissionConflict
	}
	for i, a := range delivered {
		expected := strings.TrimRight(t.DeliveryBase, "/") + "/v1/model-tasks/" + url.PathEscape(t.ID) + "/artifacts/" + strconv.Itoa(i)
		if a.Source != expected || !ownedartifact.ValidReceipt(t.ID, i, entries[i]) {
			return ErrChannelAdmissionConflict
		}
	}
	var receipt struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		QuoteJSON     string `json:"quoteJson"`
		ChargedMicros *int64 `json:"chargedMicros"`
	}
	if json.Unmarshal([]byte(t.BillingReceiptJSON), &receipt) != nil || receipt.ID != t.BillingOperationID || receipt.Status != "settled" || receipt.ChargedMicros == nil || *receipt.ChargedMicros < 0 || *receipt.ChargedMicros > maximumSafeMicros {
		return ErrMigrationProbePending
	}
	return nil
}
func probeBillingRequest(t *NativeTask, a MigrationProbeAuthorization) MigrationProbeBillingRequest {
	return MigrationProbeBillingRequest{a.Command.Seal.OperationID, a.Command.Seal.From.KeyDigest, t.BillingOperationID, t.ID, t.UpstreamID, t.GroupID, "fal", t.Endpoint, t.CredentialScope, t.KeyFingerprint, shadowDigest(t.PrepaymentQuoteJSON)}
}
func probeBillValid(t *NativeTask, b MigrationProbeBillingEvidence, a MigrationProbeAuthorization) bool {
	var receipt struct {
		ChargedMicros *int64 `json:"chargedMicros"`
	}
	_ = json.Unmarshal([]byte(t.BillingReceiptJSON), &receipt)
	return b.Binding == probeBillingRequest(t, a) && b.EvidenceID != "" && len(b.EvidenceID) <= 256 && fullIdentityDigest(b.ProvenanceHash) && fullIdentityDigest(b.AccountMembershipEvidenceHash) && b.AccountMembershipEvidenceKind == "fal-workspace-key-membership-v1" && b.Currency == "USD" && b.CostMicros >= 0 && b.CostMicros <= maximumSafeMicros && b.CustomerDueMicros >= 0 && b.CustomerDueMicros <= maximumSafeMicros && b.ChargedMicros == b.CustomerDueMicros && b.SettledAtMs > 0 && receipt.ChargedMicros != nil && *receipt.ChargedMicros == b.ChargedMicros
}

// FinalizeMigrationProbe performs no network operation while holding a DB lock.
// A nil reader is an explicit unsupported provenance boundary, not a guessed
// legacy fallback. The actual internal HTTP reader is installed by the controller.
func (s *ChannelAdmissionStore) FinalizeMigrationProbe(ctx context.Context, op string, reader MigrationProbeBillingReader) (out VerifiedMigrationProbe, err error) {
	observation := s.ReadMigrationProbe(ctx, op)
	if observation.State != "pending" || observation.Observation == nil {
		return out, ErrChannelAdmissionConflict
	}
	if observation.Observation.TaskStatus != "completed" {
		return out, ErrMigrationProbePending
	}
	var initial NativeTask
	if e := s.DB.WithContext(ctx).First(&initial, "id=?", observation.Observation.TaskID).Error; e != nil {
		return out, e
	}
	if e := probeDeliveryAndSettlement(&initial); e != nil {
		return out, e
	}
	if reader == nil {
		return out, ErrMigrationProbeMissingProvenance
	}
	authorization, e := s.MigrationProbeAuthorization(ctx, op)
	if e != nil {
		return out, e
	}
	bill, e := reader(ctx, probeBillingRequest(&initial, authorization))
	if e != nil {
		return out, e
	}
	if !probeBillValid(&initial, bill, authorization) {
		return out, ErrChannelAdmissionConflict
	}
	proof, proofErr := s.deployment(ctx)
	if proofErr != nil {
		return out, proofErr
	}
	expected := probeTaskFactHash(&initial)
	err = s.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		if e := probeRuntimeStorageReady(tx); e != nil {
			return e
		}
		resource, e := migrationTargetByOperation(tx, op, false)
		if e != nil || resource == nil {
			return ErrChannelAdmissionConflict
		}
		run, a, _, e := loadProbeRun(tx, op)
		if e != nil {
			return e
		}
		if e = probeResource(tx, c, a, s.MigrationFeatures); e != nil {
			return e
		}
		observation, e := probeObservationTx(tx, c, op)
		if e != nil || observation.State != "pending" {
			return ErrChannelAdmissionConflict
		}
		var task NativeTask
		if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&task, "id=?", run.TaskID).Error; e != nil {
			return e
		}
		if probeTaskFactHash(&task) != expected {
			return ErrChannelAdmissionConflict
		}
		if e = probeDeliveryAndSettlement(&task); e != nil {
			return e
		}
		if !probeBillValid(&task, bill, a) {
			return ErrChannelAdmissionConflict
		}
		now, e := lockDeploymentProof(tx, c, proof, s.Executor)
		if e != nil {
			return e
		}
		if bill.SettledAtMs > now.UnixMilli() {
			return ErrChannelAdmissionConflict
		}
		evidenceHash, e := admissionHash([]any{"native-migration-probe-v1", run.AuthorizationHash, run.SpecHash, run.UseID, run.TaskHash, expected, bill})
		if e != nil {
			return e
		}
		// Lifetime is bounded by the actual deployment grant already held, never a
		// browser duration or a new task TTL. Expiration is immutable and not renewed.
		until := proof.ValidUntilMs
		if proof.RevocationFreshUntilMs < until {
			until = proof.RevocationFreshUntilMs
		}
		out = VerifiedMigrationProbe{RequestHash: run.CommandHash, EvidenceHash: evidenceHash, StorageGeneration: c.StorageGeneration, IssuedAtMs: now.UnixMilli(), ValidUntilMs: until}
		if until <= out.IssuedAtMs {
			return ErrChannelAdmissionUnavailable
		}
		if run.EvidenceJSON != "" {
			var old VerifiedMigrationProbe
			if json.Unmarshal([]byte(run.EvidenceJSON), &old) != nil || run.EvidenceTaskHash != expected || old.EvidenceHash != evidenceHash || old.RequestHash != run.CommandHash || old.StorageGeneration != c.StorageGeneration {
				return ErrChannelAdmissionConflict
			}
			out = old
			if e = finalizeMigrationProbe(tx, out); e != nil {
				return e
			}
			return finalizeAdmissionProof(tx, c, proof, c.Revision)
		}
		fact, e := out.Fingerprint()
		if e != nil {
			return e
		}
		raw, _ := json.Marshal(out)
		if e = tx.Create(&ChannelMigrationProbeRecord{EvidenceHash: evidenceHash, FactHash: fact}).Error; e != nil {
			return e
		}
		res := tx.Model(run).Where("evidence_json='' AND authorization_hash=?", run.AuthorizationHash).UpdateColumns(map[string]any{"evidence_json": string(raw), "evidence_task_hash": expected})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrChannelAdmissionConflict
		}
		if e = finalizeMigrationProbe(tx, out); e != nil {
			return e
		}
		return finalizeAdmissionProof(tx, c, proof, c.Revision)
	})
	return
}

// PrepareMigrationFromProbe uses only this operation's immutable produced fact.
// It does not expose Seal/Commit: post-seal reauthorization/reverse and a real
// fleet/cache/recovery-watermark issuer remain separate rollout prerequisites.
func (s *ChannelAdmissionStore) PrepareMigrationFromProbe(ctx context.Context, op string, reader MigrationProbeBillingReader) (*ChannelMigrationPrepared, error) {
	return s.prepareMigrationFromProbe(ctx, op, reader, false, "")
}

func (s *ChannelAdmissionStore) PrepareReverseMigrationFromProbe(ctx context.Context, op string, reader MigrationProbeBillingReader) (*ChannelMigrationPrepared, error) {
	return s.prepareMigrationFromProbe(ctx, op, reader, true, "")
}

// PrepareBoundMigrationFromProbe selects the explicit kind from the persisted,
// exact authorization. HTTP callers cannot pass a replacement command/kind.
func (s *ChannelAdmissionStore) PrepareBoundMigrationFromProbe(ctx context.Context, op, authorizationHash string, reader MigrationProbeBillingReader) (*ChannelMigrationPrepared, error) {
	if !ValidMigrationReadOperationID(op) || !fullIdentityDigest(authorizationHash) {
		return nil, ErrChannelAdmissionConflict
	}
	a, e := s.MigrationProbeAuthorization(ctx, op)
	if e != nil {
		return nil, e
	}
	hash, _ := admissionHash(a)
	if hash != authorizationHash || !validMigrationResourceKind(a.Command) {
		return nil, ErrChannelAdmissionConflict
	}
	return s.prepareMigrationFromProbe(ctx, op, reader, reverseMigration(a.Command), authorizationHash)
}

func (s *ChannelAdmissionStore) prepareMigrationFromProbe(ctx context.Context, op string, reader MigrationProbeBillingReader, reverse bool, expectedAuthorization string) (*ChannelMigrationPrepared, error) {
	original, e := s.MigrationProbeAuthorization(ctx, op)
	if e != nil {
		return nil, e
	}
	originalHash, _ := admissionHash(original)
	if (expectedAuthorization != "" && originalHash != expectedAuthorization) || reverse != reverseMigration(original.Command) || !validMigrationResourceKind(original.Command) {
		return nil, ErrChannelAdmissionConflict
	}
	var fact VerifiedMigrationProbe
	var found bool
	read := s.migrationReadTransaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) (MigrationReadResult, error) {
		observed, e := probeObservationTx(tx, c, op)
		if e != nil {
			return MigrationReadResult{}, e
		}
		if observed.State != "pending" || observed.Observation == nil || observed.Observation.TaskStatus != "completed" {
			return MigrationReadResult{}, ErrChannelAdmissionConflict
		}
		run, _, _, e := loadProbeRun(tx, op, true)
		if e != nil {
			return MigrationReadResult{}, e
		}
		if run.EvidenceJSON != "" {
			var current NativeTask
			if tx.First(&current, "id=?", run.TaskID).Error != nil || probeTaskFactHash(&current) != run.EvidenceTaskHash {
				return MigrationReadResult{}, ErrChannelAdmissionConflict
			}
			if json.Unmarshal([]byte(run.EvidenceJSON), &fact) != nil || fact.RequestHash != run.CommandHash || fact.StorageGeneration != run.StorageGeneration {
				return MigrationReadResult{}, ErrChannelAdmissionConflict
			}
			found = true
		}
		return migrationReadState("ready", ""), nil
	})
	if read.State != "ready" {
		return nil, ErrChannelAdmissionConflict
	}
	if !found {
		fact, e = s.FinalizeMigrationProbe(ctx, op, reader)
		if e != nil {
			return nil, e
		}
	}
	a, e := s.MigrationProbeAuthorization(ctx, op)
	if e != nil {
		return nil, e
	}
	currentHash, _ := admissionHash(a)
	if currentHash != originalHash {
		return nil, ErrChannelAdmissionConflict
	}
	copyStore := *s
	copyStore.LoadMigrationProbe = func(_ context.Context, r ChannelMigrationRequest) (VerifiedMigrationProbe, error) {
		hash, _ := admissionHash(r)
		if hash != fact.RequestHash {
			return VerifiedMigrationProbe{}, ErrChannelAdmissionConflict
		}
		return fact, nil
	}
	if reverse {
		return copyStore.PrepareReverseMigration(ctx, a.Command)
	}
	return copyStore.PrepareMigration(ctx, a.Command)
}
