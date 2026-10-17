package model_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"math"
	"strings"
	"testing"
	"time"
)

func TestMigrationReadAtomicFactsAndAbsence(t *testing.T) {
	f := newAdmissionFixture(t)
	req, _ := migrationFixture(t, f, 3)
	ctx := context.Background()
	check := func(state string) {
		got := f.store.ReadPreparedMigrationFacts(ctx, req.Seal.OperationID)
		require.Equal(t, "ready", got.State)
		require.Equal(t, state, got.Readback.Receipt.State)
		require.Equal(t, req.Seal.From, got.Readback.From)
		require.Equal(t, req.Seal.To, got.Readback.To)
		require.Equal(t, []string{"example"}, *got.ConfigModels)
		require.True(t, got.Readback.FromEnabled)
		require.Equal(t, state == "committed", got.Readback.ToEnabled)
		raw, err := json.Marshal(got)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "synthetic-old")
		require.NotContains(t, string(raw), "9007199254740993123456789")
	}
	protocol := f.store.ReadMigrationCommandProtocol(ctx)
	require.Equal(t, "ready", protocol.State)
	require.False(t, *protocol.ExecutionReady)
	require.True(t, protocol.Protocol.AtomicPins)
	require.NotEmpty(t, protocol.Blockers)
	missing := f.store.ReadPreparedMigrationFacts(ctx, "absent-op")
	require.Equal(t, "absent", missing.State)
	require.Equal(t, "absent-op", missing.OperationID)
	require.Equal(t, f.proof.Epoch, missing.Epoch)
	require.Equal(t, f.proof.StorageGeneration, missing.StorageGeneration)
	require.Equal(t, "0", missing.Revision)
	snap := f.store.ReadMigrationSnapshotFacts(ctx, req.Seal.From, req.Seal.To)
	require.Equal(t, "ready", snap.State)
	require.Equal(t, req.ExpectedPinHash, snap.Snapshot.PinHash)
	_, err := f.store.PrepareMigration(ctx, req)
	require.NoError(t, err)
	check("prepared")
	seal, err := f.store.SealPrepared(ctx, req)
	require.NoError(t, err)
	check("sealed")
	receipt, err := f.store.CommitPreparedPins(ctx, req, seal.Revision)
	require.NoError(t, err)
	check("committed")
	got := f.store.ReadPreparedMigrationFacts(ctx, req.Seal.OperationID)
	require.Equal(t, receipt.AfterPinHash, got.Readback.PinHash)
	require.Equal(t, fmt.Sprint(receipt.CommitRevision), got.Readback.Receipt.CommitRevision)
	require.Equal(t, fmt.Sprint(seal.RecordedAtMs), got.Readback.CurrentHead.RecordedAtMs)
	// SQL-level read-only enforcement; not merely an unchanged-row observation.
	pool, err := f.store.DB.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	require.NoError(t, f.store.DB.Exec("PRAGMA query_only=ON").Error)
	require.Equal(t, "ready", f.store.ReadMigrationCommandProtocol(ctx).State)
	require.Equal(t, "ready", f.store.ReadMigrationSnapshotFacts(ctx, req.Seal.From, req.Seal.To).State)
	require.Equal(t, "ready", f.store.ReadPreparedMigrationFacts(ctx, req.Seal.OperationID).State)
	require.Equal(t, "absent", f.store.ReadPreparedMigrationFacts(ctx, "absent-op").State)
}
func TestMigrationReadRejectsPartialAndDrift(t *testing.T) {
	for _, mode := range []string{"deleted-receipt", "missing-probe", "revoked-probe", "missing-barrier", "wrong-head", "missing-receipt", "new-member", "changed-pin", "deleted-member", "epoch", "storage", "abandoned"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdmissionFixture(t)
			req, probe := migrationFixture(t, f, 3)
			ctx := context.Background()
			db := f.store.DB
			_, err := f.store.PrepareMigration(ctx, req)
			require.NoError(t, err)
			if mode == "abandoned" {
				_, err = f.store.AbandonPrepared(ctx, req.Seal.OperationID, probe.RequestHash)
				require.NoError(t, err)
			} else {
				seal, err := f.store.SealPrepared(ctx, req)
				require.NoError(t, err)
				_, err = f.store.CommitPreparedPins(ctx, req, seal.Revision)
				require.NoError(t, err)
			}
			switch mode {
			case "deleted-receipt":
				require.NoError(t, db.Exec("DELETE FROM channel_migration_prepareds").Error)
			case "missing-probe":
				require.NoError(t, db.Exec("DELETE FROM channel_migration_probe_records").Error)
			case "revoked-probe":
				require.NoError(t, db.Exec("UPDATE channel_migration_probe_records SET revoked=?", true).Error)
			case "missing-barrier":
				require.NoError(t, db.Exec("DELETE FROM channel_admission_barriers").Error)
			case "wrong-head":
				require.NoError(t, db.Exec("UPDATE channel_admission_families SET operation_id='different-op'").Error)
			case "missing-receipt":
				require.NoError(t, db.Exec("UPDATE channel_migration_prepareds SET state='sealed',commit_revision=0,committed_at_ms=0,after_pin_hash=''").Error)
			case "new-member":
				require.NoError(t, db.Exec("INSERT INTO model_configs(model,config,plugin) VALUES ('new','{}','{}')").Error)
			case "changed-pin":
				require.NoError(t, db.Exec("UPDATE model_configs SET config='{}'").Error)
			case "deleted-member":
				require.NoError(t, db.Exec("DELETE FROM model_configs").Error)
			case "epoch":
				require.NoError(t, db.Exec("UPDATE channel_admission_controls SET epoch='new-epoch'").Error)
			case "storage":
				require.NoError(t, db.Exec("UPDATE channel_admission_controls SET storage_generation='new-storage'").Error)
			}
			result := f.store.ReadPreparedMigrationFacts(ctx, req.Seal.OperationID)
			require.NotEqual(t, "ready", result.State)
			require.NotEqual(t, "absent", result.State)
			require.Nil(t, result.Readback)
		})
	}
}
func TestMigrationReadReadinessAndDecimalBoundary(t *testing.T) {
	f := newAdmissionFixture(t)
	req, _ := migrationFixture(t, f, 3)
	ctx := context.Background()
	require.NoError(t, f.store.DB.Exec("UPDATE channel_admission_controls SET revision=?", int64(math.MaxInt64)).Error)
	snap := f.store.ReadMigrationSnapshotFacts(ctx, req.Seal.From, req.Seal.To)
	require.Equal(t, "ready", snap.State)
	require.Equal(t, "9223372036854775807", snap.Snapshot.Revision)
	require.NoError(t, f.store.DB.Migrator().DropTable(&model.ChannelMigrationProbeRecord{}))
	require.Equal(t, "unsupported", f.store.ReadPreparedMigrationFacts(ctx, "missing").State)
	require.Equal(t, "unsupported", f.store.ReadMigrationCommandProtocol(ctx).State)
	f.store.LogDB = nil
	require.Equal(t, "unsupported", f.store.ReadPreparedMigrationFacts(ctx, "missing").State)
}

func TestMigrationReadPreparedAfterPriorFamilyHead(t *testing.T) {
	f := newAdmissionFixture(t)
	first, _ := migrationFixture(t, f, 3)
	ctx := context.Background()
	_, err := f.store.PrepareMigration(ctx, first)
	require.NoError(t, err)
	seal, err := f.store.SealPrepared(ctx, first)
	require.NoError(t, err)
	_, err = f.store.CommitPreparedPins(ctx, first, seal.Revision)
	require.NoError(t, err)
	require.NoError(t, f.store.DB.Model(&model.Channel{}).Where("id = ?", 5).Update("status", model.ChannelStatusDisabled).Error)
	snap, err := f.store.ReadMigrationSnapshot(ctx, f.identities[2], f.identities[4])
	require.NoError(t, err)
	next := model.ChannelMigrationRequest{Seal: f.seal("next-operation", snap.Revision, 3, 5, ""), FromCredentialID: "new", ToCredentialID: "newest", ExpectedPinHash: snap.PinHash, ProjectionHash: snap.ProjectionHash, FamilyHash: snap.FamilyHash}
	raw, err := json.Marshal(next)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	probe := model.VerifiedMigrationProbe{RequestHash: hex.EncodeToString(sum[:]), EvidenceHash: strings.Repeat("b", 64), StorageGeneration: f.proof.StorageGeneration, IssuedAtMs: time.Now().Add(-time.Minute).UnixMilli(), ValidUntilMs: time.Now().Add(time.Minute).UnixMilli()}
	fact, err := probe.Fingerprint()
	require.NoError(t, err)
	require.NoError(t, f.store.DB.Create(&model.ChannelMigrationProbeRecord{EvidenceHash: probe.EvidenceHash, FactHash: fact}).Error)
	f.store.LoadMigrationProbe = func(context.Context, model.ChannelMigrationRequest) (model.VerifiedMigrationProbe, error) {
		return probe, nil
	}
	_, err = f.store.PrepareMigration(ctx, next)
	require.NoError(t, err)
	got := f.store.ReadPreparedMigrationFacts(ctx, next.Seal.OperationID)
	require.Equal(t, "ready", got.State)
	require.Equal(t, "prepared", got.Readback.Receipt.State)
	require.Nil(t, got.Readback.CurrentHead, "null means this operation has no seal head; prior family heads remain legitimate")
	require.Equal(t, next.FamilyHash, got.Readback.FamilyHash)
	var families int64
	require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionFamily{}).Where("operation_id = ?", first.Seal.OperationID).Count(&families).Error)
	require.Positive(t, families)
}
