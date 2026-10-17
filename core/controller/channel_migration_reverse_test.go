package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// The real engine sends HTTP only to this existing local synthetic supplier;
// reverse commands stay server-only and billing provenance is explicitly synthetic.
func TestReverseMigrationActualNativeProbeToCutover(t *testing.T) {
	for _, mode := range []string{"success", "unknown", "wrong-billing"} {
		t.Run(mode, func(t *testing.T) {
			f := newProbeHTTPFixture(t)
			forward, ok := probeCommand(f.body.Command)
			require.True(t, ok)
			raw, _ := json.Marshal(forward)
			hash := sha256.Sum256(raw)
			proof, _ := f.store.LoadDeployment(t.Context())
			fact := model.VerifiedMigrationProbe{RequestHash: hex.EncodeToString(hash[:]), EvidenceHash: strings.Repeat("a", 64), StorageGeneration: proof.StorageGeneration, IssuedAtMs: proof.IssuedAtMs, ValidUntilMs: proof.ValidUntilMs}
			ph, e := fact.Fingerprint()
			require.NoError(t, e)
			require.NoError(t, f.db.Create(&model.ChannelMigrationProbeRecord{EvidenceHash: fact.EvidenceHash, FactHash: ph}).Error)
			f.store.LoadMigrationProbe = func(context.Context, model.ChannelMigrationRequest) (model.VerifiedMigrationProbe, error) {
				return fact, nil
			}
			_, e = f.store.PrepareMigration(t.Context(), forward)
			require.NoError(t, e)
			_, e = f.store.CutoverPrepared(t.Context(), forward)
			require.NoError(t, e)
			require.NoError(t, f.db.AutoMigrate(&model.ChannelMigrationRetiredTargetResource{}))
			inv := f.store.ReadMigrationInventory(t.Context(), f.store.MigrationFeatures)
			require.Equal(t, "ready", inv.State, inv.ReasonCode)
			snap, e := f.store.ReadMigrationSnapshot(t.Context(), forward.Seal.To, forward.Seal.From)
			require.NoError(t, e)
			req := model.ChannelMigrationRequest{TargetResourceKind: model.MigrationReuseRetiredTarget, Seal: model.ChannelSealRequest{OperationID: "reverse-native-runtime", Epoch: snap.Epoch, ExpectedRevision: snap.Revision, From: forward.Seal.To, To: forward.Seal.From, RevertOf: forward.Seal.OperationID}, FromCredentialID: forward.ToCredentialID, ToCredentialID: forward.FromCredentialID, ExpectedPinHash: snap.PinHash, ProjectionHash: snap.ProjectionHash, FamilyHash: snap.FamilyHash}
			r, e := f.store.PrepareRetiredTargetResource(t.Context(), model.MigrationRetiredTargetRequest{Version: 1, Command: req, SourceSnapshotHash: inv.SourceSnapshotHash})
			require.NoError(t, e)
			a := model.MigrationProbeAuthorization{Command: req, ResourceRequestHash: r.RequestHash, CheckpointHash: r.CheckpointHash, Model: f.body.Model, Input: f.body.Input, GroupID: f.body.GroupID, TokenID: f.body.TokenID, MaxPrepaidMicros: 600000}
			h, e := f.store.BeginMigrationProbe(t.Context(), a)
			require.NoError(t, e)
			spec, e := h.Spec()
			require.NoError(t, e)
			ch, e := f.deps.native.channel(spec.ChannelID)
			require.NoError(t, e)
			if mode == "unknown" {
				f.loseProviderReply = true
			}
			local := *f.engine
			local.Provider = f.deps.native.provider(ch)
			queued, e := local.Submit(t.Context(), spec.TaskID, spec.GroupID, spec.TokenID, spec.Input, nativetask.Plan{MigrationProbe: h, AdmissionIdentityHash: spec.IdentityHash, KeyFingerprint: spec.KeyDigest, Contract: spec.Contract, ChannelID: spec.ChannelID, Endpoint: spec.Endpoint, CredentialScope: spec.CredentialScope, QuoteJSON: spec.QuoteJSON, DeliveryBase: spec.DeliveryBase, Direct: true})

			if mode == "unknown" {
				require.Error(t, e)
				observed := f.store.ReadMigrationProbe(t.Context(), req.Seal.OperationID)
				require.Equal(t, "unknown", observed.State)
				_, e = f.store.PrepareReverseMigrationFromProbe(t.Context(), req.Seal.OperationID, nil)
				require.Error(t, e)
				_, e = f.store.BeginMigrationProbe(t.Context(), a)
				require.NoError(t, e)
				_, e = local.Submit(t.Context(), spec.TaskID, spec.GroupID, spec.TokenID, spec.Input, nativetask.Plan{MigrationProbe: h, AdmissionIdentityHash: spec.IdentityHash, KeyFingerprint: spec.KeyDigest, Contract: spec.Contract, ChannelID: spec.ChannelID, Endpoint: spec.Endpoint, CredentialScope: spec.CredentialScope, QuoteJSON: spec.QuoteJSON, DeliveryBase: spec.DeliveryBase, Direct: true})
				require.Error(t, e)
				require.Equal(t, 1, f.providerCalls)
				return
			}
			require.NoError(t, e)
			require.Equal(t, "queued", queued.Status)
			completed, e := f.engine.Poll(t.Context(), spec.TaskID, spec.GroupID, spec.TokenID, func(context.Context, *model.NativeTask) (nativetask.Poller, error) {
				return probeCompletedPoller{}, nil
			}, func(_ context.Context, id string, index int, _ string) (ownedartifact.Receipt, error) {
				return ownedartifact.Receipt{Key: fmt.Sprintf("native-results/%s/%d/%s.bin", id, index, strings.Repeat("a", 64)), SHA256: strings.Repeat("b", 64), Size: 23}, nil
			})
			require.NoError(t, e)
			require.Equal(t, "completed", completed.Status)
			frozen := *completed
			reader := func(_ context.Context, b model.MigrationProbeBillingRequest) (model.MigrationProbeBillingEvidence, error) {
				return model.MigrationProbeBillingEvidence{Binding: b, EvidenceID: "reverse-local-bill", ProvenanceHash: strings.Repeat("d", 64), AccountMembershipEvidenceHash: strings.Repeat("e", 64), AccountMembershipEvidenceKind: "fal-workspace-key-membership-v1", Currency: "USD", CostMicros: 600000, CustomerDueMicros: 900000, ChargedMicros: 900000, SettledAtMs: time.Now().Add(-time.Second).UnixMilli()}, nil
			}
			_, e = f.store.PrepareReverseMigrationFromProbe(t.Context(), req.Seal.OperationID, nil)
			require.ErrorIs(t, e, model.ErrMigrationProbeMissingProvenance)

			if mode == "wrong-billing" {
				_, e = f.store.PrepareReverseMigrationFromProbe(t.Context(), req.Seal.OperationID, func(ctx context.Context, r model.MigrationProbeBillingRequest) (model.MigrationProbeBillingEvidence, error) {
					bill, e := reader(ctx, r)
					bill.Binding.KeyDigest = strings.Repeat("0", 64)
					return bill, e
				})
				require.Error(t, e)
				var n int64
				require.NoError(t, f.db.Model(&model.ChannelMigrationPrepared{}).Where("operation_id=?", req.Seal.OperationID).Count(&n).Error)
				require.Zero(t, n)
				require.Equal(t, 1, f.providerCalls)
				return
			}
			fact, e = f.store.FinalizeMigrationProbe(t.Context(), req.Seal.OperationID, reader)
			require.NoError(t, e)
			prepared, e := f.store.PrepareReverseMigrationFromProbe(t.Context(), req.Seal.OperationID, nil)
			require.NoError(t, e)
			require.Equal(t, "prepared", prepared.State)
			result, e := f.store.CutoverReversePrepared(t.Context(), req)
			require.NoError(t, e)
			require.Equal(t, "committed", result.State)
			var after model.NativeTask
			require.NoError(t, f.db.First(&after, "id=?", spec.TaskID).Error)
			require.Equal(t, frozen.PrepaymentQuoteJSON, after.PrepaymentQuoteJSON)
			require.Equal(t, frozen.BillingReceiptJSON, after.BillingReceiptJSON)
			require.Equal(t, frozen.FrozenContract, after.FrozenContract)
			require.Equal(t, 1, f.providerCalls)
			var uses []model.ChannelAdmissionUse
			require.NoError(t, f.db.Find(&uses).Error)
			require.Len(t, uses, 1)
			require.Equal(t, "open", uses[0].State)
			// New resource kind is not silently discarded by the existing HTTP decoder.
			input := f.body.Command
			input.TargetResourceKind = model.MigrationReuseRetiredTarget
			_, ok = probeCommand(input)
			require.False(t, ok)

		})
	}
}
