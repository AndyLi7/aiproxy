package model_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"gorm.io/gorm"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func reverseSyntheticProbe(t *testing.T, f *admissionFixture, req model.ChannelMigrationRequest) *model.VerifiedMigrationProbe {
	t.Helper()
	raw, e := json.Marshal(req)
	require.NoError(t, e)
	h := sha256.Sum256(raw)
	evidence := sha256.Sum256(append([]byte("reverse-local-fixture:"), raw...))
	p := &model.VerifiedMigrationProbe{RequestHash: hex.EncodeToString(h[:]), EvidenceHash: hex.EncodeToString(evidence[:]), StorageGeneration: f.proof.StorageGeneration, IssuedAtMs: time.Now().Add(-time.Minute).UnixMilli(), ValidUntilMs: time.Now().Add(time.Minute).UnixMilli()}
	ph, e := p.Fingerprint()
	require.NoError(t, e)
	require.NoError(t, f.store.DB.Create(&model.ChannelMigrationProbeRecord{EvidenceHash: p.EvidenceHash, FactHash: ph}).Error)
	f.store.LoadMigrationProbe = func(context.Context, model.ChannelMigrationRequest) (model.VerifiedMigrationProbe, error) {
		return *p, nil
	}
	return p
}
func reverseRequest(t *testing.T, f *admissionFixture, previous model.ChannelMigrationRequest, op string) model.MigrationRetiredTargetRequest {
	t.Helper()
	snap, e := f.store.ReadMigrationSnapshot(t.Context(), previous.Seal.To, previous.Seal.From)
	require.NoError(t, e)
	inv := f.store.ReadMigrationInventory(t.Context(), f.store.MigrationFeatures)
	require.Equal(t, "ready", inv.State, inv.ReasonCode)
	req := model.ChannelMigrationRequest{TargetResourceKind: model.MigrationReuseRetiredTarget, Seal: model.ChannelSealRequest{OperationID: op, Epoch: snap.Epoch, ExpectedRevision: snap.Revision, From: previous.Seal.To, To: previous.Seal.From, RevertOf: previous.Seal.OperationID}, FromCredentialID: previous.ToCredentialID, ToCredentialID: previous.FromCredentialID, ExpectedPinHash: snap.PinHash, ProjectionHash: snap.ProjectionHash, FamilyHash: snap.FamilyHash}
	return model.MigrationRetiredTargetRequest{Version: 1, Command: req, SourceSnapshotHash: inv.SourceSnapshotHash}
}
func reverseFixture(t *testing.T, f *admissionFixture) (model.MigrationProbeAuthorization, model.MigrationRetiredTargetRequest) {
	t.Helper()
	a, _ := probeFixture(t, f)
	reverseSyntheticProbe(t, f, a.Command)
	_, e := f.store.PrepareMigration(t.Context(), a.Command)
	require.NoError(t, e)
	_, e = f.store.CutoverPrepared(t.Context(), a.Command)
	require.NoError(t, e)
	installRetiredDraft(t, f.store.DB)
	return a, reverseRequest(t, f, a.Command, "reverse-first")
}
func TestReverseCutoverResourceAndCycles(t *testing.T) {
	f := newAdmissionFixture(t)
	_, q := reverseFixture(t, f)
	db := f.store.DB
	var beforeCount int64
	require.NoError(t, db.Model(&model.Channel{}).Count(&beforeCount).Error)
	// Repeated inverse operations reuse existing IDs, each with immutable evidence.
	for i := 0; i < 3; i++ {
		r, e := f.store.PrepareRetiredTargetResource(t.Context(), q)
		require.NoError(t, e)
		replay, e := f.store.PrepareRetiredTargetResource(t.Context(), q)
		require.NoError(t, e)
		require.Equal(t, r, replay)
		reverseSyntheticProbe(t, f, q.Command)
		prepared, e := f.store.PrepareReverseMigration(t.Context(), q.Command)
		require.NoError(t, e)
		require.Equal(t, "prepared", prepared.State)
		_, e = f.store.SealPrepared(t.Context(), q.Command)
		require.Error(t, e)
		_, e = f.store.CutoverPrepared(t.Context(), q.Command)
		require.Error(t, e)
		got, e := f.store.CutoverReversePrepared(t.Context(), q.Command)
		require.NoError(t, e)
		require.Equal(t, "committed", got.State)
		replayPrepared, e := f.store.CutoverReversePrepared(t.Context(), q.Command)
		require.NoError(t, e)
		require.Equal(t, got, replayPrepared)
		read, e := f.store.ReadPreparedMigration(t.Context(), q.Command.Seal.OperationID)
		require.NoError(t, e)
		require.Equal(t, got, read)
		facts := f.store.ReadPreparedMigrationFacts(t.Context(), q.Command.Seal.OperationID)
		require.Equal(t, "ready", facts.State, facts.Reason)
		require.Equal(t, model.MigrationReuseRetiredTarget, facts.Readback.Request.TargetResourceKind)
		_, e = f.store.CancelRetiredTargetResource(t.Context(), r.OperationID, r.RequestHash, r.CheckpointHash)
		require.Error(t, e)
		require.Equal(t, model.ChannelStatusEnabled, migrationStatus(t, db, q.Command.Seal.From.ChannelID))
		require.Equal(t, model.ChannelStatusEnabled, migrationStatus(t, db, q.Command.Seal.To.ChannelID))
		var count int64
		require.NoError(t, db.Model(&model.Channel{}).Count(&count).Error)
		require.Equal(t, beforeCount, count)
		q = reverseRequest(t, f, q.Command, fmt.Sprintf("reverse-next-%d", i))
	}
	var count int64
	require.NoError(t, db.Model(&model.ChannelMigrationRetiredTargetResource{}).Count(&count).Error)
	require.EqualValues(t, 3, count)
}
func TestReverseResourceRejectsDrift(t *testing.T) {
	for _, mode := range []string{"wrong-retirement", "wrong-kind", "old-revision", "wrong-credential", "target-disabled", "source-inventory", "missing-schema", "partial-schema", "historical-sealed", "pending-conflict", "changed-price", "checkpoint", "wrong-version"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdmissionFixture(t)
			a, q := reverseFixture(t, f)
			db := f.store.DB
			switch mode {
			case "wrong-retirement":
				q.Command.Seal.RevertOf = "older"
			case "wrong-kind":
				q.Command.TargetResourceKind = ""
			case "old-revision":
				q.Command.Seal.ExpectedRevision--
			case "wrong-credential":
				q.Command.ToCredentialID = "wrong"
			case "target-disabled":
				require.NoError(t, db.Model(&model.Channel{}).Where("id=?", q.Command.Seal.To.ChannelID).UpdateColumn("status", model.ChannelStatusDisabled).Error)
			case "source-inventory":
				q.SourceSnapshotHash = q.Command.FamilyHash
			case "missing-schema":
				require.NoError(t, db.Migrator().DropTable(&model.ChannelMigrationRetiredTargetResource{}))
			case "partial-schema":
				require.NoError(t, db.Exec("ALTER TABLE channel_migration_retired_target_resources DROP COLUMN retirement_request_hash").Error)
				require.False(t, db.Migrator().HasColumn(&model.ChannelMigrationRetiredTargetResource{}, "retirement_request_hash"))
			case "historical-sealed":
				require.NoError(t, db.Model(&model.ChannelMigrationPrepared{}).Where("operation_id=?", a.Command.Seal.OperationID).UpdateColumns(map[string]any{"state": "sealed", "commit_revision": 0, "committed_at_ms": 0, "after_pin_hash": ""}).Error)
			}
			if mode == "pending-conflict" || mode == "changed-price" || mode == "checkpoint" || mode == "wrong-version" {
				r, e := f.store.PrepareRetiredTargetResource(t.Context(), q)
				require.NoError(t, e)
				switch mode {
				case "pending-conflict":
					q.Command.Seal.OperationID = "another-inverse"
					var c model.ChannelAdmissionControl
					require.NoError(t, db.First(&c, 1).Error)
					q.Command.Seal.ExpectedRevision = c.Revision
				case "changed-price":
					require.NoError(t, db.Exec("UPDATE model_configs SET config=config||' '").Error)
				case "checkpoint":
					require.NoError(t, db.Model(r).UpdateColumn("checkpoint_hash", q.Command.FamilyHash).Error)
				case "wrong-version":
					require.NoError(t, db.Model(r).UpdateColumn("version", 99).Error)
				}
				if mode != "pending-conflict" {
					reverseSyntheticProbe(t, f, q.Command)
					_, e = f.store.PrepareReverseMigration(t.Context(), q.Command)
					require.Error(t, e)
					return
				}
			}
			_, e := f.store.PrepareRetiredTargetResource(t.Context(), q)
			require.Error(t, e)
		})
	}
}
func TestReverseCancelRetainsEnabledTarget(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(fmt.Sprint(prepared), func(t *testing.T) {
			f := newAdmissionFixture(t)
			_, q := reverseFixture(t, f)
			r, e := f.store.PrepareRetiredTargetResource(t.Context(), q)
			require.NoError(t, e)
			if prepared {
				reverseSyntheticProbe(t, f, q.Command)
				_, e = f.store.PrepareReverseMigration(t.Context(), q.Command)
				require.NoError(t, e)
			}
			out, e := f.store.CancelRetiredTargetResource(t.Context(), r.OperationID, r.RequestHash, r.CheckpointHash)
			require.NoError(t, e)
			require.Equal(t, "cancelled", out.LifecycleState)
			again, e := f.store.CancelRetiredTargetResource(t.Context(), r.OperationID, r.RequestHash, r.CheckpointHash)
			require.NoError(t, e)
			require.Equal(t, out, again)
			read, e := f.store.ReadRetiredTargetResource(t.Context(), r.OperationID, r.CheckpointHash)
			require.NoError(t, e)
			require.Equal(t, out, read)
			require.Equal(t, model.ChannelStatusEnabled, migrationStatus(t, f.store.DB, q.Command.Seal.To.ChannelID))
			if !prepared {
				reverseSyntheticProbe(t, f, q.Command)
			}
			_, e = f.store.PrepareReverseMigration(t.Context(), q.Command)
			require.Error(t, e)
		})
	}
}
func TestReverseActualRestrictedProbeReservation(t *testing.T) {
	f := newAdmissionFixture(t)
	a, q := reverseFixture(t, f)
	r, e := f.store.PrepareRetiredTargetResource(t.Context(), q)
	require.NoError(t, e)
	a.Command = q.Command
	a.ResourceRequestHash = r.RequestHash
	a.CheckpointHash = r.CheckpointHash
	h, e := f.store.BeginMigrationProbe(t.Context(), a)
	require.NoError(t, e)
	_, _, e = f.store.Admit(t.Context(), admissionRequest("ordinary-retired", q.Command.Seal.To), nil)
	require.ErrorIs(t, e, model.ErrChannelAdmissionSealed)
	task := probeNativeTask(t, h)
	saved, created, e := model.ReserveMigrationProbeTask(t.Context(), f.store.DB, h, task)
	require.NoError(t, e)
	require.True(t, created)
	require.Equal(t, q.Command.Seal.To.ChannelID, saved.ChannelID)
	read := f.store.ReadMigrationProbe(t.Context(), q.Command.Seal.OperationID)
	require.Equal(t, "pending", read.State, read.Reason)
	_, _, e = model.ReserveMigrationProbeTask(t.Context(), f.store.DB, h, task)
	require.Error(t, e)
	_, e = f.store.CancelRetiredTargetResource(t.Context(), r.OperationID, r.RequestHash, r.CheckpointHash)
	require.NoError(t, e)
	read = f.store.ReadMigrationProbe(t.Context(), q.Command.Seal.OperationID)
	require.Equal(t, "cancelled", read.State, read.Reason)
}

func TestReverseSameKeyAndFrozenRawPrices(t *testing.T) {
	f := newAdmissionFixture(t)
	req, _ := inventoryFixture(t, f)
	db := f.store.DB
	require.NoError(t, db.Model(&model.Channel{}).Where("id=3").UpdateColumn("status", model.ChannelStatusEnabled).Error)
	require.NoError(t, db.Model(&model.Channel{}).Where("id=2").UpdateColumn("status", model.ChannelStatusDisabled).Error)
	req.Seal.To = f.identities[1]
	req.ToCredentialID = req.FromCredentialID
	snap, e := f.store.ReadMigrationSnapshot(t.Context(), req.Seal.From, req.Seal.To)
	require.NoError(t, e)
	req.ExpectedPinHash = snap.PinHash
	req.ProjectionHash = snap.ProjectionHash
	req.FamilyHash = snap.FamilyHash
	before := migrationConfig(t, db)
	reverseSyntheticProbe(t, f, req)
	_, e = f.store.PrepareMigration(t.Context(), req)
	require.NoError(t, e)
	_, e = f.store.CutoverPrepared(t.Context(), req)
	require.NoError(t, e)
	require.NoError(t, db.AutoMigrate(&model.ChannelMigrationRetiredTargetResource{}))
	q := reverseRequest(t, f, req, "same-key-return")
	_, e = f.store.PrepareRetiredTargetResource(t.Context(), q)
	require.NoError(t, e)
	reverseSyntheticProbe(t, f, q.Command)
	_, e = f.store.PrepareReverseMigration(t.Context(), q.Command)
	require.NoError(t, e)
	_, e = f.store.CutoverReversePrepared(t.Context(), q.Command)
	require.NoError(t, e)
	after := migrationConfig(t, db)
	var old, new map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(before), &old))
	require.NoError(t, json.Unmarshal([]byte(after), &new))
	require.Equal(t, old["unknown"], new["unknown"])
	for _, value := range []string{"9007199254740993123456789", "1.230000000000000000009", "123456789012345678901", "987654321098765432109"} {
		require.Contains(t, after, value)
	}
	require.Equal(t, model.ChannelStatusEnabled, migrationStatus(t, db, 1))
	require.Equal(t, model.ChannelStatusEnabled, migrationStatus(t, db, 2))
}
func TestReverseStackHistoricalReceiptSeparateFromCurrentHead(t *testing.T) {
	f := newAdmissionFixture(t)
	a, _ := reverseFixture(t, f)
	db := f.store.DB
	var old model.ChannelMigrationShadowResource
	require.NoError(t, db.First(&old, "operation_id=?", a.Command.Seal.OperationID).Error)
	var source model.MigrationShadowRequest
	require.NoError(t, json.Unmarshal([]byte(old.RequestJSON), &source))
	inv := f.store.ReadMigrationInventory(t.Context(), nil)
	require.Equal(t, "ready", inv.State)
	source.OperationID = "forward-second"
	source.Source = model.MigrationShadowSource{SourceSnapshotHash: inv.SourceSnapshotHash, Epoch: inv.Epoch, Revision: inv.Revision, StorageGeneration: inv.StorageGeneration, From: a.Command.Seal.To}
	key := "synthetic-stack-third"
	hash := sha256.Sum256([]byte(key))
	source.Target.CredentialDigest = hex.EncodeToString(hash[:])
	source.Target.CredentialID = "stack-third"
	source.Target.Projection.Name = "Stack third"
	created := f.store.CreateMigrationShadow(t.Context(), source, key, nil)
	require.Equal(t, "ready", created.State, created.Reason)
	snap, e := f.store.ReadMigrationSnapshot(t.Context(), created.Receipt.From, created.Receipt.Shadow)
	require.NoError(t, e)
	second := model.ChannelMigrationRequest{Seal: model.ChannelSealRequest{OperationID: source.OperationID, Epoch: snap.Epoch, ExpectedRevision: snap.Revision, From: created.Receipt.From, To: created.Receipt.Shadow}, FromCredentialID: a.Command.ToCredentialID, ToCredentialID: source.Target.CredentialID, ExpectedPinHash: snap.PinHash, ProjectionHash: snap.ProjectionHash, FamilyHash: snap.FamilyHash}
	reverseSyntheticProbe(t, f, second)
	_, e = f.store.PrepareMigration(t.Context(), second)
	require.NoError(t, e)
	_, e = f.store.CutoverPrepared(t.Context(), second)
	require.NoError(t, e)
	jump := reverseRequest(t, f, a.Command, "bad-skip")
	jump.Command.Seal.From = second.Seal.To
	jump.Command.FromCredentialID = second.ToCredentialID
	_, e = f.store.PrepareRetiredTargetResource(t.Context(), jump)
	require.Error(t, e)
	for i, forward := range []model.ChannelMigrationRequest{second, a.Command} {
		q := reverseRequest(t, f, forward, fmt.Sprintf("undo-stack-%d", i))
		_, e = f.store.PrepareRetiredTargetResource(t.Context(), q)
		require.NoError(t, e)
		reverseSyntheticProbe(t, f, q.Command)
		_, e = f.store.PrepareReverseMigration(t.Context(), q.Command)
		require.NoError(t, e)
		_, e = f.store.CutoverReversePrepared(t.Context(), q.Command)
		require.NoError(t, e)
	}
	var count int64
	require.NoError(t, db.Model(&model.Channel{}).Count(&count).Error)
	require.EqualValues(t, 3, count)
	// Original forward is historical and must not itself become executable again.
	_, e = f.store.ReadPreparedMigration(t.Context(), a.Command.Seal.OperationID)
	require.Error(t, e)
}

func installRetiredDraft(t *testing.T, db *gorm.DB) {
	t.Helper()
	raw, e := os.ReadFile("../../docs/sql/b2-retired-target-resource-draft.sql")
	require.NoError(t, e)
	lines := []string{}
	for _, l := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "--") {
			lines = append(lines, l)
		}
	}
	for _, statement := range strings.Split(strings.Join(lines, "\n"), ";") {
		if strings.TrimSpace(statement) != "" {
			require.NoError(t, db.Exec(statement).Error)
		}
	}
}

func TestReverseCutoverRevisionBounds(t *testing.T) {
	for _, rev := range []int64{math.MaxInt64 - 2, math.MaxInt64 - 1, math.MaxInt64} {
		t.Run(fmt.Sprint(rev), func(t *testing.T) {
			f := newAdmissionFixture(t)
			_, q := reverseFixture(t, f)
			_, e := f.store.PrepareRetiredTargetResource(t.Context(), q)
			require.NoError(t, e)
			reverseSyntheticProbe(t, f, q.Command)
			_, e = f.store.PrepareReverseMigration(t.Context(), q.Command)
			require.NoError(t, e)
			require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionControl{}).Where("id=1").UpdateColumn("revision", rev).Error)
			got, e := f.store.CutoverReversePrepared(t.Context(), q.Command)
			if rev > math.MaxInt64-2 {
				require.Error(t, e)
				var row model.ChannelMigrationPrepared
				require.NoError(t, f.store.DB.First(&row, "operation_id=?", q.Command.Seal.OperationID).Error)
				require.Equal(t, "prepared", row.State)
				return
			}
			require.NoError(t, e)
			require.EqualValues(t, math.MaxInt64, got.CommitRevision)
			again, e := f.store.CutoverReversePrepared(t.Context(), q.Command)
			require.NoError(t, e)
			require.Equal(t, got, again)
			require.Equal(t, "ready", f.store.ReadPreparedMigrationFacts(t.Context(), q.Command.Seal.OperationID).State)
		})
	}
}
func TestReversePendingResourceBlocksOrdinaryMutations(t *testing.T) {
	f := newAdmissionFixture(t)
	_, q := reverseFixture(t, f)
	_, e := f.store.PrepareRetiredTargetResource(t.Context(), q)
	require.NoError(t, e)
	old, logs := model.DB, model.LogDB
	model.DB, model.LogDB = f.store.DB, f.store.DB
	t.Cleanup(func() { model.DB, model.LogDB = old, logs })
	restore := model.ConfigureChannelAdmissionRuntime(f.store.Executor, f.store.LoadDeployment)
	defer restore()
	for _, id := range []int{q.Command.Seal.From.ChannelID, q.Command.Seal.To.ChannelID} {
		require.Error(t, model.UpdateChannelStatusByID(id, model.ChannelStatusDisabled))
		require.Error(t, model.UpdateChannelStatusByID(id, model.ChannelStatusEnabled))
		name := "ordinary-edit"
		require.Error(t, model.UpdateChannel(&model.Channel{ID: id}, &model.ChannelPatch{Name: &name}))
	}
	require.Error(t, model.DeleteModelConfig("test/native::text-to-video"))
	var cfg model.ModelConfig
	require.NoError(t, f.store.DB.First(&cfg).Error)
	require.Error(t, model.SaveModelConfig(cfg))
}
func TestReverseForwardEmptyKindHashCompatibility(t *testing.T) {
	q := model.ChannelMigrationRequest{}
	raw, e := json.Marshal(q)
	require.NoError(t, e)
	require.NotContains(t, string(raw), "targetResourceKind")
	require.Equal(t, `{"seal":{"operationId":"","epoch":"","expectedRevision":0,"from":{"channelId":0,"keyDigest":"","identityHash":""},"to":{"channelId":0,"keyDigest":"","identityHash":""}},"fromCredentialId":"","toCredentialId":"","expectedPinHash":"","projectionHash":"","familyHash":""}`, string(raw))
}
