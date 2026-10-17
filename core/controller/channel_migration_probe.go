package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
)

// Only server bootstrap supplies the actual issuer. There is intentionally no
// test manifest, issuer setting, browser proof, or Seal/Commit handler here.
type migrationProbeDependencies struct {
	store   func() (*model.ChannelAdmissionStore, error)
	native  nativeTrialDependencies
	billing model.MigrationProbeBillingReader
}

func migrationProbeDefaults() migrationProbeDependencies {
	return migrationProbeDependencies{
		store: func() (*model.ChannelAdmissionStore, error) {
			return model.MigrationProbeRuntimeStore(model.DB, model.LogDB, gatewayRuntimeFeatures())
		}, native: nativeTrialDefaults(),
		// The internal app coordinator validates original context and actual settled
		// facts, then appends current workspace attribution without rewriting money.
		billing: balance.ReadMigrationProbeBilling,
	}
}

type migrationProbeBody struct {
	Version             int                        `json:"version"`
	Command             model.MigrationReadRequest `json:"command"`
	ResourceRequestHash string                     `json:"resourceRequestHash"`
	CheckpointHash      string                     `json:"checkpointHash"`
	Model               string                     `json:"model"`
	Input               json.RawMessage            `json:"input"`
	GroupID             string                     `json:"groupId"`
	TokenID             int                        `json:"tokenId"`
	MaxPrepaidMicros    string                     `json:"maxPrepaidMicros"`
}

func probeHTTPReject(c *gin.Context) {
	middleware.ErrorResponse(c, 400, "invalid_migration_probe_request")
}
func probeHTTPError(c *gin.Context, e error) {
	state, reason := "unavailable", "migration_probe_unavailable"
	if errors.Is(e, model.ErrMigrationProbePending) {
		state = "pending"
		reason = "task_or_billing_pending"
	}
	if errors.Is(e, model.ErrMigrationProbeMissingProvenance) {
		state = "pending"
		reason = "missing_provenance"
	}
	if errors.Is(e, model.ErrChannelAdmissionConflict) {
		state = "conflict"
		reason = "probe_binding_conflict"
	}
	middleware.SuccessResponse(c, gin.H{"version": 1, "source": "gateway-migration-probe-v1", "operationId": c.Param("id"), "state": state, "reason": reason, "epoch": "", "storageGeneration": "", "revision": "", "observation": nil})
}
func probeDecode(c *gin.Context, out any) bool {
	raw, e := io.ReadAll(io.LimitReader(c.Request.Body, 384*1024+1))
	if e != nil || len(raw) == 0 || len(raw) > 384*1024 {
		return false
	}
	// The bounded native decoder rejects duplicate keys and excessive nesting,
	// including free-form native input. No validation error echoes the payload.
	if !model.ValidMigrationProbeJSON(raw) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(out) == nil
}
func probeCommand(r model.MigrationReadRequest) (model.ChannelMigrationRequest, bool) {
	revision, e := strconv.ParseInt(r.Seal.ExpectedRevision, 10, 64)
	if e != nil || revision < 0 || strconv.FormatInt(revision, 10) != r.Seal.ExpectedRevision {
		return model.ChannelMigrationRequest{}, false
	}
	return model.ChannelMigrationRequest{Seal: model.ChannelSealRequest{OperationID: r.Seal.OperationID, Epoch: r.Seal.Epoch, ExpectedRevision: revision, From: r.Seal.From, To: r.Seal.To, RevertOf: r.Seal.RevertOf}, FromCredentialID: r.FromCredentialID, ToCredentialID: r.ToCredentialID, ExpectedPinHash: r.ExpectedPinHash, ProjectionHash: r.ProjectionHash, FamilyHash: r.FamilyHash}, true
}
func (d migrationProbeDependencies) create(c *gin.Context) {
	op := c.Param("id")
	var b migrationProbeBody
	if !model.ValidMigrationReadOperationID(op) || !probeDecode(c, &b) || b.Version != 1 || b.Command.Seal.OperationID != op {
		probeHTTPReject(c)
		return
	}
	command, ok := probeCommand(b.Command)
	budget, e := strconv.ParseInt(b.MaxPrepaidMicros, 10, 64)
	if !ok || e != nil || budget < 0 || strconv.FormatInt(budget, 10) != b.MaxPrepaidMicros {
		probeHTTPReject(c)
		return
	}
	store, e := d.store()
	if e != nil {
		probeHTTPError(c, e)
		return
	}
	engine, ok := d.native.engine()
	if !ok {
		probeHTTPError(c, model.ErrChannelAdmissionUnavailable)
		return
	}
	h, e := store.BeginMigrationProbe(c.Request.Context(), model.MigrationProbeAuthorization{Command: command, ResourceRequestHash: b.ResourceRequestHash, CheckpointHash: b.CheckpointHash, Model: b.Model, Input: b.Input, GroupID: b.GroupID, TokenID: b.TokenID, MaxPrepaidMicros: budget})
	if e != nil {
		probeHTTPError(c, e)
		return
	}
	observation := store.ReadMigrationProbe(c.Request.Context(), op)
	if observation.State != "pending" || observation.Observation == nil || observation.Observation.UseID != "" {
		middleware.SuccessResponse(c, observation)
		return
	}
	spec, e := h.Spec()
	if e != nil {
		probeHTTPError(c, e)
		return
	}
	ch, e := d.native.channel(spec.ChannelID)
	if e != nil || ch == nil {
		probeHTTPError(c, model.ErrChannelAdmissionUnavailable)
		return
	}
	// Resolve actual credential from the stored exact identity; this is never a
	// browser key, quote, endpoint, or provider selector.
	identity, e := model.ChannelIdentityOf(ch)
	if e != nil || identity.ChannelID != spec.ChannelID || identity.KeyDigest != spec.KeyDigest || identity.IdentityHash != spec.IdentityHash {
		probeHTTPError(c, model.ErrChannelAdmissionConflict)
		return
	}
	local := *engine
	local.Provider = d.native.provider(ch)
	// The original published maximum is frozen for this administrative trial.
	// This is a hold authorization, not a ceiling on final actual-cost settlement.
	local.InputMeter = false
	_, submitErr := local.Submit(c.Request.Context(), spec.TaskID, spec.GroupID, spec.TokenID, spec.Input, nativetask.Plan{MigrationProbe: h, AdmissionIdentityHash: spec.IdentityHash, KeyFingerprint: spec.KeyDigest, Contract: spec.Contract, ChannelID: spec.ChannelID, Endpoint: spec.Endpoint, CredentialScope: spec.CredentialScope, QuoteJSON: spec.QuoteJSON, DeliveryBase: spec.DeliveryBase, Direct: true})
	result := store.ReadMigrationProbe(c.Request.Context(), op)
	if submitErr != nil && result.Observation == nil {
		probeHTTPError(c, submitErr)
		return
	}
	middleware.SuccessResponse(c, result)
}
func (d migrationProbeDependencies) read(c *gin.Context) {
	if !migrationReadNoBody(c) {
		return
	}
	if !model.ValidMigrationReadOperationID(c.Param("id")) {
		probeHTTPReject(c)
		return
	}
	// Readback requires storage, not an online issuer, so an interrupted run is
	// observable even while deployment authorization is unavailable.
	store := migrationReadStore()
	middleware.SuccessResponse(c, store.ReadMigrationProbe(c.Request.Context(), c.Param("id")))
}
func (d migrationProbeDependencies) prepare(c *gin.Context) {
	op := c.Param("id")
	var body struct {
		Version           int    `json:"version"`
		AuthorizationHash string `json:"authorizationHash"`
	}
	if !model.ValidMigrationReadOperationID(op) || !probeDecode(c, &body) || body.Version != 1 || len(body.AuthorizationHash) != 64 {
		probeHTTPReject(c)
		return
	}
	s, e := d.store()
	if e != nil {
		probeHTTPError(c, e)
		return
	}
	seen := s.ReadMigrationProbe(c.Request.Context(), op)
	if seen.Observation == nil || seen.Observation.AuthorizationHash != body.AuthorizationHash {
		probeHTTPError(c, model.ErrChannelAdmissionConflict)
		return
	}
	if _, e = s.PrepareMigrationFromProbe(c.Request.Context(), op, d.billing); e != nil {
		probeHTTPError(c, e)
		return
	}
	// Separate readback may fail after a committed response loss. That remains
	// unavailable; callers recover the exact operation using the existing reader.
	middleware.SuccessResponse(c, s.ReadPreparedMigrationFacts(c.Request.Context(), op))
}
func CreateMigrationProbe(c *gin.Context)  { migrationProbeDefaults().create(c) }
func GetMigrationProbe(c *gin.Context)     { migrationProbeDefaults().read(c) }
func PrepareMigrationProbe(c *gin.Context) { migrationProbeDefaults().prepare(c) }
