package model_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"os"
	"strings"
	"testing"
	"time"
)

func lifecycleFixture(t *testing.T, f *admissionFixture) (model.MigrationShadowRequest, *model.MigrationShadowReceipt, model.ChannelMigrationRequest) {
	t.Helper()
	source := shadowFixture(t, f)
	ctx := context.Background()
	got := f.store.CreateMigrationShadow(ctx, source, shadowTestKey, nil)
	require.Equal(t, "ready", got.State)
	snapshot, e := f.store.ReadMigrationSnapshot(ctx, got.Receipt.From, got.Receipt.Shadow)
	require.NoError(t, e)
	req := model.ChannelMigrationRequest{Seal: model.ChannelSealRequest{OperationID: source.OperationID, Epoch: snapshot.Epoch, ExpectedRevision: snapshot.Revision, From: got.Receipt.From, To: got.Receipt.Shadow}, FromCredentialID: "old", ToCredentialID: source.Target.CredentialID, ExpectedPinHash: snapshot.PinHash, ProjectionHash: snapshot.ProjectionHash, FamilyHash: snapshot.FamilyHash}
	lifecycleProbe(t, f, req)
	return source, got.Receipt, req
}
func lifecycleProbe(t *testing.T, f *admissionFixture, req model.ChannelMigrationRequest) {
	raw, e := json.Marshal(req)
	require.NoError(t, e)
	sum := sha256.Sum256(raw)
	p := model.VerifiedMigrationProbe{RequestHash: hex.EncodeToString(sum[:]), EvidenceHash: strings.Repeat("c", 64), StorageGeneration: f.proof.StorageGeneration, IssuedAtMs: time.Now().Add(-time.Minute).UnixMilli(), ValidUntilMs: time.Now().Add(time.Minute).UnixMilli()}
	hash, e := p.Fingerprint()
	require.NoError(t, e)
	require.NoError(t, f.store.DB.Save(&model.ChannelMigrationProbeRecord{EvidenceHash: p.EvidenceHash, FactHash: hash}).Error)
	f.store.LoadMigrationProbe = func(context.Context, model.ChannelMigrationRequest) (model.VerifiedMigrationProbe, error) {
		return p, nil
	}
}
func lifecycleRow(t *testing.T, f *admissionFixture, op string) model.ChannelMigrationShadowResource {
	var r model.ChannelMigrationShadowResource
	require.NoError(t, f.store.DB.First(&r, "operation_id=?", op).Error)
	return r
}
func TestMigrationShadowLifecycleCancelRetainsAndDoesNotNeedSource(t *testing.T) {
	for _, mode := range []string{"created", "config-drift", "family-drift", "prepared", "abandon"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdmissionFixture(t)
			source, receipt, req := lifecycleFixture(t, f)
			ctx := context.Background()
			db := f.store.DB
			if mode == "prepared" || mode == "abandon" {
				_, e := f.store.PrepareMigration(ctx, req)
				require.NoError(t, e)
			}
			if mode == "config-drift" {
				require.NoError(t, db.Exec("UPDATE model_configs SET rpm=99").Error)
			}
			if mode == "family-drift" {
				_, e := f.store.Seal(ctx, f.seal("other-family", 1, 1, 2, ""))
				require.NoError(t, e)
			}
			var uses, probes int64
			require.NoError(t, db.Model(&model.ChannelAdmissionUse{}).Count(&uses).Error)
			require.NoError(t, db.Model(&model.ChannelMigrationProbeRecord{}).Count(&probes).Error)
			if mode == "abandon" {
				p, e := f.store.AbandonPrepared(ctx, req.Seal.OperationID, lifecycleRow(t, f, source.OperationID).PreparedRequestHash)
				require.NoError(t, e)
				require.Equal(t, "abandoned", p.State)
			}
			cancelled, e := f.store.CancelMigrationShadow(ctx, source.OperationID, receipt.RequestHash, receipt.CheckpointHash)
			require.NoError(t, e)
			require.Equal(t, "cancelled", cancelled.State)
			replay, e := f.store.CancelMigrationShadow(ctx, source.OperationID, receipt.RequestHash, receipt.CheckpointHash)
			require.NoError(t, e)
			require.Equal(t, cancelled, replay)
			read, e := f.store.ReadMigrationShadowLifecycle(ctx, source.OperationID, receipt.CheckpointHash)
			require.NoError(t, e)
			require.Equal(t, cancelled, read)
			require.Equal(t, model.ChannelStatusDisabled, migrationStatus(t, db, receipt.Shadow.ChannelID))
			require.Equal(t, int64(1), countShadows(t, f))
			var n int64
			require.NoError(t, db.Model(&model.Channel{}).Count(&n).Error)
			require.Equal(t, int64(6), n)
			require.NoError(t, db.Model(&model.ChannelAdmissionUse{}).Count(&n).Error)
			require.Equal(t, uses, n)
			require.NoError(t, db.Model(&model.ChannelMigrationProbeRecord{}).Count(&n).Error)
			require.Equal(t, probes, n)
			require.Equal(t, "conflict", f.store.CreateMigrationShadow(ctx, source, shadowTestKey, nil).State)
			_, e = f.store.PrepareMigration(ctx, req)
			require.Error(t, e)
			_, e = f.store.SealPrepared(ctx, req)
			require.Error(t, e)
			require.Equal(t, receipt.CheckpointHash, lifecycleRow(t, f, source.OperationID).CheckpointHash)
		})
	}
}
func TestMigrationShadowLifecycleConsumeSealCommit(t *testing.T) {
	f := newAdmissionFixture(t)
	source, receipt, req := lifecycleFixture(t, f)
	ctx := context.Background()
	p, e := f.store.PrepareMigration(ctx, req)
	require.NoError(t, e)
	require.Equal(t, receipt.CheckpointHash, p.ShadowCheckpointHash)
	require.Equal(t, "consumed", lifecycleRow(t, f, source.OperationID).LifecycleState)
	again, e := f.store.PrepareMigration(ctx, req)
	require.NoError(t, e)
	require.Equal(t, p, again)
	_, e = f.store.AbandonPrepared(ctx, source.OperationID, strings.Repeat("f", 64))
	require.Error(t, e)
	// A legitimate unrelated family transition advances revision but not our frozen source.
	_, e = f.store.Seal(ctx, f.seal("unrelated-after-consume", 2, 5, 3, ""))
	require.NoError(t, e)
	seal, e := f.store.SealPrepared(ctx, req)
	require.NoError(t, e)
	_, e = f.store.CancelMigrationShadow(ctx, source.OperationID, receipt.RequestHash, receipt.CheckpointHash)
	require.Error(t, e)
	// Original created checkpoint no longer authorizes after our family seal.
	require.Equal(t, "conflict", f.store.ReadMigrationShadow(ctx, source.OperationID, nil).State)
	_, e = f.store.ReadPreparedMigration(ctx, source.OperationID)
	require.NoError(t, e)
	committed, e := f.store.CommitPreparedPins(ctx, req, seal.Revision)
	require.NoError(t, e)
	require.Equal(t, "committed", committed.State)
	_, e = f.store.CancelMigrationShadow(ctx, source.OperationID, receipt.RequestHash, receipt.CheckpointHash)
	require.Error(t, e)
	_, e = f.store.ReadPreparedMigration(ctx, source.OperationID)
	require.NoError(t, e)
	require.Contains(t, migrationConfig(t, f.store.DB), "123456789012345678901")
}
func TestMigrationShadowLifecycleMissingHistoryOrMismatchRefuses(t *testing.T) {
	for _, mode := range []string{"unknown", "wrong-op", "changed-credential", "source-change", "missing-resource", "missing-prepared", "missing-backlink", "corrupt-probe-link", "cancel-rollback", "prepare-rollback"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdmissionFixture(t)
			source, receipt, req := lifecycleFixture(t, f)
			ctx := context.Background()
			db := f.store.DB
			switch mode {
			case "unknown":
				require.NoError(t, db.Exec("UPDATE channel_migration_shadow_resources SET lifecycle_state='unknown',lifecycle_revision=0").Error)
			case "wrong-op":
				req.Seal.OperationID = "wrong-op"
				lifecycleProbe(t, f, req)
			case "changed-credential":
				req.ToCredentialID = "foreign"
				lifecycleProbe(t, f, req)
			case "source-change":
				require.NoError(t, db.Exec("UPDATE model_configs SET rpm=99").Error)
			case "missing-resource", "missing-prepared", "missing-backlink", "corrupt-probe-link":
				_, e := f.store.PrepareMigration(ctx, req)
				require.NoError(t, e)
				switch mode {
				case "missing-resource":
					require.NoError(t, db.Exec("DELETE FROM channel_migration_shadow_resources").Error)
				case "missing-prepared":
					require.NoError(t, db.Exec("DELETE FROM channel_migration_prepareds").Error)
				case "missing-backlink":
					require.NoError(t, db.Exec("UPDATE channel_migration_prepareds SET shadow_checkpoint_hash=''").Error)
				case "corrupt-probe-link":
					require.NoError(t, db.Exec("UPDATE channel_migration_shadow_resources SET prepared_probe_fact_hash=?", strings.Repeat("e", 64)).Error)
				}
				_, e = f.store.SealPrepared(ctx, req)
				require.Error(t, e)
				_, e = f.store.ReadPreparedMigration(ctx, source.OperationID)
				require.Error(t, e)
			case "prepare-rollback":
				require.NoError(t, db.Exec("CREATE TRIGGER deny_consume BEFORE UPDATE ON channel_migration_shadow_resources WHEN NEW.lifecycle_state='consumed' BEGIN SELECT RAISE(ABORT,'fixture'); END").Error)
			case "cancel-rollback":
				_, e := f.store.PrepareMigration(ctx, req)
				require.NoError(t, e)
				require.NoError(t, db.Exec("CREATE TRIGGER deny_cancel BEFORE UPDATE ON channel_migration_shadow_resources WHEN NEW.lifecycle_state='cancelled' BEGIN SELECT RAISE(ABORT,'fixture'); END").Error)
				_, e = f.store.CancelMigrationShadow(ctx, source.OperationID, receipt.RequestHash, receipt.CheckpointHash)
				require.Error(t, e)
				var p model.ChannelMigrationPrepared
				require.NoError(t, db.First(&p).Error)
				require.Equal(t, "prepared", p.State)
				require.Equal(t, "consumed", lifecycleRow(t, f, source.OperationID).LifecycleState)
				return
			}
			_, e := f.store.PrepareMigration(ctx, req)
			require.Error(t, e)
			if mode == "prepare-rollback" {
				var n int64
				require.NoError(t, db.Model(&model.ChannelMigrationPrepared{}).Count(&n).Error)
				require.Zero(t, n)
				require.Equal(t, "created", lifecycleRow(t, f, source.OperationID).LifecycleState)
			}
		})
	}
}

func TestMigrationShadowLifecycleSameKeyAndRetainedProtection(t *testing.T) {
	f := newAdmissionFixture(t)
	source := shadowFixture(t, f)
	source.Target.CredentialDigest = source.Source.From.KeyDigest
	source.Target.CredentialID = "old"
	ctx := context.Background()
	got := f.store.CreateMigrationShadow(ctx, source, "synthetic-old", nil)
	require.Equal(t, "ready", got.State)
	snapshot, e := f.store.ReadMigrationSnapshot(ctx, got.Receipt.From, got.Receipt.Shadow)
	require.NoError(t, e)
	req := model.ChannelMigrationRequest{Seal: model.ChannelSealRequest{OperationID: source.OperationID, Epoch: snapshot.Epoch, ExpectedRevision: snapshot.Revision, From: got.Receipt.From, To: got.Receipt.Shadow}, FromCredentialID: "old", ToCredentialID: "old", ExpectedPinHash: snapshot.PinHash, ProjectionHash: snapshot.ProjectionHash, FamilyHash: snapshot.FamilyHash}
	lifecycleProbe(t, f, req)
	_, e = f.store.PrepareMigration(ctx, req)
	require.NoError(t, e)
	_, e = f.store.CancelMigrationShadow(ctx, source.OperationID, got.Receipt.RequestHash, got.Receipt.CheckpointHash)
	require.NoError(t, e)
	_, _, e = f.store.Admit(ctx, admissionRequest("old-still-usable", source.Source.From), nil)
	require.NoError(t, e)
	var old model.Channel
	require.NoError(t, f.store.DB.First(&old, 1).Error)
	require.Equal(t, "synthetic-old", old.Key)
	// Normal delete/edit cannot repurpose a cancelled resource.
	oldDB, oldLog := model.DB, model.LogDB
	model.DB, model.LogDB = f.store.DB, f.store.DB
	defer func() { model.DB, model.LogDB = oldDB, oldLog }()
	require.Error(t, model.DeleteChannelByID(got.Receipt.Shadow.ChannelID))
	name := "takeover"
	require.Error(t, model.UpdateChannel(&model.Channel{ID: got.Receipt.Shadow.ChannelID}, &model.ChannelPatch{Name: &name}))
}
func TestMigrationShadowLifecycleV1UpgradeDoesNotInventHistory(t *testing.T) {
	f := newAdmissionFixture(t)
	source, receipt, req := lifecycleFixture(t, f)
	db := f.store.DB
	before := lifecycleRow(t, f, source.OperationID)
	for _, column := range []string{"lifecycle_state", "lifecycle_revision", "prepared_request_hash", "prepared_probe_hash", "prepared_probe_fact_hash", "prepared_at_ms", "cancelled_at_ms"} {
		require.NoError(t, db.Exec("ALTER TABLE channel_migration_shadow_resources DROP COLUMN "+column).Error)
	}
	require.NoError(t, db.Exec("ALTER TABLE channel_migration_prepareds DROP COLUMN shadow_checkpoint_hash").Error)
	require.NoError(t, db.Exec("UPDATE channel_migration_shadow_protocols SET version=1").Error)
	_, e := f.store.CancelMigrationShadow(context.Background(), source.OperationID, receipt.RequestHash, receipt.CheckpointHash)
	require.Error(t, e)
	raw, e := os.ReadFile("../../docs/sql/b2-shadow-lifecycle-v2-upgrade-sqlite-draft.sql")
	require.NoError(t, e)
	require.NoError(t, db.Exec(string(raw)).Error)
	after := lifecycleRow(t, f, source.OperationID)
	require.Equal(t, "unknown", after.LifecycleState)
	require.Zero(t, after.LifecycleRevision)
	require.Equal(t, before.RequestJSON, after.RequestJSON)
	require.Equal(t, before.CheckpointHash, after.CheckpointHash)
	require.Equal(t, before.SourceSnapshotHash, after.SourceSnapshotHash)
	_, e = f.store.CancelMigrationShadow(context.Background(), source.OperationID, receipt.RequestHash, receipt.CheckpointHash)
	require.Error(t, e)
	_, e = f.store.PrepareMigration(context.Background(), req)
	require.Error(t, e)
}
func TestMigrationShadowLifecycleReadOnlyAndUnavailable(t *testing.T) {
	for _, state := range []string{"created", "consumed", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			f := newAdmissionFixture(t)
			source, receipt, req := lifecycleFixture(t, f)
			ctx := context.Background()
			db := f.store.DB
			if state == "consumed" {
				_, e := f.store.PrepareMigration(ctx, req)
				require.NoError(t, e)
			}
			if state == "cancelled" {
				_, e := f.store.CancelMigrationShadow(ctx, source.OperationID, receipt.RequestHash, receipt.CheckpointHash)
				require.NoError(t, e)
			}
			require.NoError(t, db.Exec("PRAGMA query_only=ON").Error)
			r, e := f.store.ReadMigrationShadowLifecycle(ctx, source.OperationID, receipt.CheckpointHash)
			require.NoError(t, e)
			require.Equal(t, state, r.State)
			_, e = f.store.ReadMigrationShadowLifecycle(ctx, "absent", receipt.CheckpointHash)
			require.Error(t, e)
			require.NoError(t, db.Exec("PRAGMA query_only=OFF").Error)
			require.NoError(t, db.Exec("UPDATE channel_migration_shadow_protocols SET version=1").Error)
			_, e = f.store.ReadMigrationShadowLifecycle(ctx, source.OperationID, receipt.CheckpointHash)
			require.Error(t, e)
		})
	}
}

func TestMigrationShadowLifecycleLegacyPreparedWithoutShadowExtension(t *testing.T) {
	f := newAdmissionFixture(t)
	req, _ := migrationFixture(t, f, 3)
	db := f.store.DB
	ctx := context.Background()
	require.NoError(t, db.Exec("ALTER TABLE channel_migration_prepareds DROP COLUMN shadow_checkpoint_hash").Error)
	p, e := f.store.PrepareMigration(ctx, req)
	require.NoError(t, e)
	require.Empty(t, p.ShadowCheckpointHash)
	seal, e := f.store.SealPrepared(ctx, req)
	require.NoError(t, e)
	_, e = f.store.CommitPreparedPins(ctx, req, seal.Revision)
	require.NoError(t, e)
	_, e = f.store.ReadPreparedMigration(ctx, req.Seal.OperationID)
	require.NoError(t, e)
}

func TestMigrationShadowLifecycleContradictionsDoNotRepair(t *testing.T) {
	for _, mode := range []string{"cancelled-prepared", "consumed-abandoned", "prepare-trigger", "cancel-trigger"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdmissionFixture(t)
			source, receipt, req := lifecycleFixture(t, f)
			db := f.store.DB
			ctx := context.Background()
			if mode == "prepare-trigger" {
				require.NoError(t, db.Exec("CREATE TRIGGER corrupt_prepare AFTER INSERT ON channel_migration_prepareds BEGIN UPDATE channel_migration_prepareds SET shadow_checkpoint_hash='' WHERE operation_id=NEW.operation_id; END").Error)
				_, e := f.store.PrepareMigration(ctx, req)
				require.Error(t, e)
				require.Equal(t, "created", lifecycleRow(t, f, source.OperationID).LifecycleState)
				var n int64
				require.NoError(t, db.Model(&model.ChannelMigrationPrepared{}).Count(&n).Error)
				require.Zero(t, n)
				return
			}
			_, e := f.store.PrepareMigration(ctx, req)
			require.NoError(t, e)
			switch mode {
			case "cancelled-prepared":
				_, e = f.store.CancelMigrationShadow(ctx, source.OperationID, receipt.RequestHash, receipt.CheckpointHash)
				require.NoError(t, e)
				require.NoError(t, db.Exec("UPDATE channel_migration_prepareds SET state='prepared'").Error)
			case "consumed-abandoned":
				require.NoError(t, db.Exec("UPDATE channel_migration_prepareds SET state='abandoned'").Error)
			case "cancel-trigger":
				require.NoError(t, db.Exec("CREATE TRIGGER corrupt_cancel AFTER UPDATE ON channel_migration_shadow_resources WHEN NEW.lifecycle_state='cancelled' BEGIN UPDATE channel_migration_prepareds SET state='prepared'; END").Error)
			}
			_, e = f.store.CancelMigrationShadow(ctx, source.OperationID, receipt.RequestHash, receipt.CheckpointHash)
			require.Error(t, e)
			if mode == "cancel-trigger" {
				require.Equal(t, "consumed", lifecycleRow(t, f, source.OperationID).LifecycleState)
				var p model.ChannelMigrationPrepared
				require.NoError(t, db.First(&p).Error)
				require.Equal(t, "prepared", p.State)
			}
		})
	}
}
