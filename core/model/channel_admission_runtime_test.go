package model_test

import (
	"context"
	"errors"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runtimeAdmissionFixture(t *testing.T) *admissionFixture {
	t.Helper()
	return runtimeAdmissionOnFixture(t, newAdmissionFixture(t))
}
func runtimeAdmissionOnFixture(t *testing.T, f *admissionFixture) *admissionFixture {
	t.Helper()
	require.NoError(t, f.store.DB.AutoMigrate(&model.NativeTask{}, &model.ImageTask{}, &model.AsyncUsageInfo{}, &model.ModelConfig{}))
	oldDB, oldLog := model.DB, model.LogDB
	model.DB, model.LogDB = f.store.DB, f.store.DB
	restore := model.ConfigureChannelAdmissionRuntime(f.store.Executor, f.store.LoadDeployment)
	t.Cleanup(func() { restore(); model.DB, model.LogDB = oldDB, oldLog })
	return f
}
func runtimeNativeTask(f *admissionFixture, id string, index int) model.NativeTask {
	identity := f.identities[index]
	return model.NativeTask{ID: id, GroupID: "group", TokenID: 1, Model: "synthetic/native", Fingerprint: strings.Repeat("a", 64), OutputSchema: `{"type":"object"}`, ChannelID: identity.ChannelID, KeyFingerprint: identity.KeyDigest, AdmissionIdentityHash: identity.IdentityHash}
}
func releaseEvidence(task *model.NativeTask) model.NativeReservationRelease {
	return model.NativeReservationRelease{GenerationID: task.AdmissionGeneration, CreatedAt: task.CreatedAt}
}
func runtimeUses(t *testing.T, db *gorm.DB) []model.ChannelAdmissionUse {
	t.Helper()
	var uses []model.ChannelAdmissionUse
	require.NoError(t, db.Order("admitted_at_ms,id").Find(&uses).Error)
	return uses
}

func TestAdmissionRuntimeNativeReleaseGenerationsAndLateReceipts(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	input := runtimeNativeTask(f, "reusable", 0)
	first, created, err := model.ReserveNativeTask(db, input)
	require.NoError(t, err)
	require.True(t, created)
	require.NotEmpty(t, first.AdmissionGeneration)
	released, err := model.ReleaseNativeReservation(db, first.ID, first.GroupID, first.TokenID, first.UpdatedAt, releaseEvidence(first))
	require.NoError(t, err)
	require.True(t, released)
	uses := runtimeUses(t, db)
	require.Len(t, uses, 1)
	require.Equal(t, "closed", uses[0].State)
	second, created, err := model.ReserveNativeTask(db, input)
	require.NoError(t, err)
	require.True(t, created)
	require.NotEqual(t, first.AdmissionGeneration, second.AdmissionGeneration)
	// Make the old caller's time cutoff permissive: generation, not time, must fence it.
	stale := releaseEvidence(first)
	stale.CreatedAt = second.CreatedAt
	released, err = model.ReleaseNativeReservation(db, second.ID, second.GroupID, second.TokenID, time.Now().Add(time.Hour), stale)
	require.ErrorIs(t, err, model.ErrChannelAdmissionConflict)
	require.False(t, released)
	replay, created, err := model.ReserveNativeTask(db, input)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, second.AdmissionGeneration, replay.AdmissionGeneration)
	uses = runtimeUses(t, db)
	require.Len(t, uses, 2)
	require.Equal(t, uses[0].ResourceKey, uses[1].ResourceKey)
	released, err = model.ReleaseNativeReservation(db, replay.ID, replay.GroupID, replay.TokenID, replay.UpdatedAt, releaseEvidence(replay))
	require.NoError(t, err)
	require.True(t, released)
	_, err = f.store.Seal(context.Background(), f.seal("sealed", 0, 1, 2, ""))
	require.NoError(t, err)
	_, _, err = model.ReserveNativeTask(db, input)
	require.ErrorIs(t, err, model.ErrChannelAdmissionSealed)
	require.Len(t, runtimeUses(t, db), 2)
}

func TestAdmissionRuntimeNativeMissingTaskCannotAuthorizeResubmit(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	input := runtimeNativeTask(f, "lost", 0)
	_, _, err := model.ReserveNativeTask(f.store.DB, input)
	require.NoError(t, err)
	require.NoError(t, f.store.DB.Delete(&model.NativeTask{}, "id = ?", input.ID).Error)
	_, _, err = model.ReserveNativeTask(f.store.DB, input)
	require.ErrorIs(t, err, model.ErrChannelAdmissionRetained)
	require.Len(t, runtimeUses(t, f.store.DB), 1)
}

func TestAdmissionRuntimeFrozenReplayIgnoresProofButChecksOwnerAndPlan(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	input := runtimeNativeTask(f, "frozen", 0)
	first, _, err := model.ReserveNativeTask(f.store.DB, input)
	require.NoError(t, err)
	_, err = f.store.Seal(context.Background(), f.seal("move", 0, 1, 2, ""))
	require.NoError(t, err)
	restore := model.ConfigureChannelAdmissionRuntime(f.store.Executor, func(context.Context) (model.VerifiedDeploymentProof, error) {
		t.Fatal("frozen replay requested proof")
		return model.VerifiedDeploymentProof{}, errors.New("unavailable")
	})
	defer restore()
	replay, created, err := model.ReserveNativeTask(f.store.DB, input)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, first.AdmissionGeneration, replay.AdmissionGeneration)
	other := input
	other.TokenID = 2
	_, _, err = model.ReserveNativeTask(f.store.DB, other)
	require.ErrorIs(t, err, model.ErrNativeTaskConflict)
	other = input
	other.ChannelID = 2
	_, _, err = model.ReserveNativeTask(f.store.DB, other)
	require.ErrorIs(t, err, model.ErrNativeTaskConflict)
}

func TestAdmissionRuntimeRejectsStaleIdentityAndRollsBackInvalidReservation(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	for _, kind := range []string{"stale", "invalid-schema", "missing-identity"} {
		t.Run(kind, func(t *testing.T) {
			input := runtimeNativeTask(f, kind, 0)
			switch kind {
			case "stale":
				input.AdmissionIdentityHash = f.identities[1].IdentityHash
			case "invalid-schema":
				input.OutputSchema = `{"type":"nonsense"}`
			case "missing-identity":
				input.AdmissionIdentityHash = ""
			}
			_, _, err := model.ReserveNativeTask(f.store.DB, input)
			require.Error(t, err)
			var count int64
			require.NoError(t, f.store.DB.Model(&model.NativeTask{}).Count(&count).Error)
			require.Zero(t, count)
			require.Empty(t, runtimeUses(t, f.store.DB))
		})
	}
}

func TestAdmissionRuntimeNativeReleaseRequiresExactLiveRecoveryOwner(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	input := runtimeNativeTask(f, "recover", 0)
	first, _, err := model.ReserveNativeTask(db, input)
	require.NoError(t, err)
	require.NoError(t, db.Model(&model.NativeTask{}).Where("id = ?", first.ID).Updates(map[string]any{"recovery_owner": "worker-1", "recovery_until": time.Now().Add(time.Minute).Unix()}).Error)
	captured, err := model.CaptureNativeReservationRelease(db, first, "worker-1")
	require.NoError(t, err)
	for _, kind := range []string{"missing", "wrong-owner", "expired"} {
		t.Run(kind, func(t *testing.T) {
			proof := captured
			if kind == "missing" {
				proof = model.NativeReservationRelease{}
			}
			if kind == "wrong-owner" {
				proof.RecoveryOwner = "worker-2"
			}
			if kind == "expired" {
				require.NoError(t, db.Model(&model.NativeTask{}).Where("id = ?", first.ID).Update("recovery_until", time.Now().Add(-time.Second).Unix()).Error)
			}
			released, err := model.ReleaseNativeReservation(db, first.ID, first.GroupID, first.TokenID, time.Now().Add(time.Hour), proof)
			require.ErrorIs(t, err, model.ErrChannelAdmissionConflict)
			require.False(t, released)
			require.Equal(t, "open", runtimeUses(t, db)[0].State)
		})
	}
	require.NoError(t, db.Model(&model.NativeTask{}).Where("id = ?", first.ID).Update("recovery_until", time.Now().Add(time.Minute).Unix()).Error)
	captured, err = model.CaptureNativeReservationRelease(db, first, "worker-1")
	require.NoError(t, err)
	released, err := model.ReleaseNativeReservation(db, first.ID, first.GroupID, first.TokenID, time.Now().Add(time.Hour), captured)
	require.NoError(t, err)
	require.True(t, released)
}

func TestAdmissionRuntimeInactiveAndPartialStorageCompatibility(t *testing.T) {
	for _, kind := range []string{"no-storage", "installed-inactive", "partial", "split"} {
		t.Run(kind, func(t *testing.T) {
			db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "runtime.db"))
			require.NoError(t, err)
			pool, _ := db.DB()
			defer pool.Close()
			require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
			oldDB, oldLog := model.DB, model.LogDB
			model.DB, model.LogDB = db, db
			defer func() { model.DB, model.LogDB = oldDB, oldLog }()
			if kind != "no-storage" {
				require.NoError(t, db.AutoMigrate(&model.ChannelAdmissionControl{}))
				require.NoError(t, db.Create(&model.ChannelAdmissionControl{ID: 1, Version: 1}).Error)
			}
			if kind == "installed-inactive" {
				require.NoError(t, db.AutoMigrate(&model.ChannelAdmissionProof{}, &model.ChannelAdmissionFamily{}, &model.ChannelAdmissionBarrier{}, &model.ChannelAdmissionUse{}))
			}
			if kind == "split" {
				model.DB = nil
			}
			input := model.NativeTask{ID: "legacy", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "legacy", OutputSchema: `{}`}
			task, created, err := model.ReserveNativeTask(db, input)
			if kind == "partial" || kind == "split" {
				require.Error(t, err)
				require.False(t, created)
				return
			}
			require.NoError(t, err)
			require.True(t, created)
			require.Empty(t, task.AdmissionGeneration)
			released, err := model.ReleaseNativeReservation(db, task.ID, task.GroupID, task.TokenID, task.UpdatedAt)
			require.NoError(t, err)
			require.True(t, released)
		})
	}
}

func TestAdmissionRuntimeChannelMutationProtectsUsesAndBarriers(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	// No task row is necessary: the upstream-accepted/outbox-not-yet-written
	// window is retained solely by its durable use.
	_, _, err := f.store.Admit(context.Background(), admissionRequest("generic-window", f.identities[0]), nil)
	require.NoError(t, err)
	channel, err := model.GetChannelByID(1)
	require.NoError(t, err)
	key := "different"
	require.ErrorIs(t, model.UpdateChannel(channel, &model.ChannelPatch{Key: &key}), model.ErrChannelAdmissionRetained)
	require.ErrorIs(t, model.DeleteChannelByID(1), model.ErrChannelAdmissionRetained)
	_, err = f.store.Seal(context.Background(), f.seal("move", 0, 1, 2, ""))
	require.NoError(t, err)
	require.ErrorIs(t, model.DeleteChannelsByIDs([]int{2, 3}), model.ErrChannelAdmissionRetained)
	name := "renamed"
	require.NoError(t, model.UpdateChannel(channel, &model.ChannelPatch{Name: &name}))
	require.NoError(t, model.UpdateChannelStatusByID(1, model.ChannelStatusDisabled))
	require.NoError(t, model.UpdateChannelStatusByID(1, model.ChannelStatusEnabled))
	_, _, err = model.ReserveNativeTask(db, runtimeNativeTask(f, "cannot-reenable", 0))
	require.ErrorIs(t, err, model.ErrChannelAdmissionSealed)
	stored, err := model.GetChannelByID(1)
	require.NoError(t, err)
	identity, err := model.ChannelIdentityOf(stored)
	require.NoError(t, err)
	require.Equal(t, f.identities[0], identity)
	// A separate unused identity is still editable when all usage sources exist.
	unused, err := model.GetChannelByID(5)
	require.NoError(t, err)
	require.NoError(t, model.UpdateChannel(unused, &model.ChannelPatch{Key: &key}))
}

func TestAdmissionRuntimeReleaseRollbackAndPersistentGenerations(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	input := runtimeNativeTask(f, "rollback", 0)
	first, _, err := model.ReserveNativeTask(db, input)
	require.NoError(t, err)
	require.NoError(t, db.Callback().Delete().After("gorm:delete").Register("test:release-failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "native_tasks" {
			tx.AddError(errors.New("synthetic crash before closure"))
		}
	}))
	released, err := model.ReleaseNativeReservation(db, first.ID, first.GroupID, first.TokenID, first.UpdatedAt, releaseEvidence(first))
	require.Error(t, err)
	require.False(t, released)
	require.NoError(t, db.Callback().Delete().Remove("test:release-failure"))
	reopened, err := model.OpenSQLite(f.path)
	require.NoError(t, err)
	pool, _ := reopened.DB()
	defer pool.Close()
	model.DB, model.LogDB = reopened, reopened
	stored, err := model.GetNativeTask(reopened, first.ID, first.GroupID, first.TokenID)
	require.NoError(t, err)
	require.Equal(t, "reserved", stored.Status)
	require.Equal(t, "open", runtimeUses(t, reopened)[0].State)
	released, err = model.ReleaseNativeReservation(reopened, first.ID, first.GroupID, first.TokenID, first.UpdatedAt, releaseEvidence(first))
	require.NoError(t, err)
	require.True(t, released)
	second, _, err := model.ReserveNativeTask(reopened, input)
	require.NoError(t, err)
	require.NotEqual(t, first.AdmissionGeneration, second.AdmissionGeneration)
	// No proof adapter is not an instruction to disable persisted protection.
	restore := model.ConfigureChannelAdmissionRuntime(f.store.Executor, nil)
	defer restore()
	released, err = model.ReleaseNativeReservation(reopened, second.ID, second.GroupID, second.TokenID, second.UpdatedAt)
	require.ErrorIs(t, err, model.ErrChannelAdmissionConflict)
	require.False(t, released)
}

func TestAdmissionRuntimeReservationAndSealSerialize(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	entered, release := make(chan struct{}), make(chan struct{})
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:hold-reservation", func(tx *gorm.DB) {
		if tx.Statement.Table == "native_tasks" {
			close(entered)
			<-release
		}
	}))
	defer db.Callback().Create().Remove("test:hold-reservation")
	reserved := make(chan error, 1)
	go func() { _, _, err := model.ReserveNativeTask(db, runtimeNativeTask(f, "first", 0)); reserved <- err }()
	<-entered
	sealed := make(chan error, 1)
	go func() { _, err := f.store.Seal(context.Background(), f.seal("move", 0, 1, 2, "")); sealed <- err }()
	select {
	case err := <-sealed:
		t.Fatalf("seal passed uncommitted reservation: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-reserved)
	require.NoError(t, <-sealed)
	require.NoError(t, db.Callback().Create().Remove("test:hold-reservation"))
	_, _, err := model.ReserveNativeTask(db, runtimeNativeTask(f, "late", 0))
	require.ErrorIs(t, err, model.ErrChannelAdmissionSealed)
	_, created, err := model.ReserveNativeTask(db, runtimeNativeTask(f, "successor", 1))
	require.NoError(t, err)
	require.True(t, created)
	require.Len(t, runtimeUses(t, db), 2)
}

func TestAdmissionRuntimeResourceUpgradeRetainsOldEvidence(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "upgrade.db"))
	require.NoError(t, err)
	pool, _ := db.DB()
	defer pool.Close()
	draft, err := os.ReadFile("../migrations/channel-admission-v1/sqlite.sql")
	require.NoError(t, err)
	old := strings.ReplaceAll(string(draft), "  resource_key VARCHAR(64) NOT NULL DEFAULT '',\n", "")
	old = strings.ReplaceAll(old, "CREATE INDEX IF NOT EXISTS idx_channel_admission_uses_resource_key ON channel_admission_uses (resource_key);\n", "")
	require.NoError(t, db.Exec(old).Error)
	require.False(t, model.ChannelAdmissionStorageReady(db))
	upgrade, err := os.ReadFile("../migrations/channel-admission-resource-v1/sqlite.sql")
	require.NoError(t, err)
	require.NoError(t, db.Exec(string(upgrade)).Error)
	require.True(t, model.ChannelAdmissionStorageReady(db))
	var c model.ChannelAdmissionControl
	require.NoError(t, db.First(&c, 1).Error)
	require.Empty(t, c.Epoch)
	require.Zero(t, c.Revision)
}
