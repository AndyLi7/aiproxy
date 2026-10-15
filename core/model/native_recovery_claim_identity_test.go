package model_test

import (
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeRecoverySameSecondReclaimUsesDistinctOwnership(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "claim.db"))
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	defer pool.Close()
	assertRecoveryClaimIdentity(t, db)
}
func TestPostgresNativeExecutionSameSecondReclaimUsesDistinctOwnership(t *testing.T) {
	assertRecoveryClaimIdentity(t, queueLockPostgres(t))
}
func assertRecoveryClaimIdentity(t *testing.T, db *gorm.DB) {
	t.Helper()
	var err error
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	_, _, err = model.ReserveNativeTask(db, model.NativeTask{ID: "same", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "f", OutputSchema: `{}`})
	require.NoError(t, err)
	now := time.Now().Truncate(time.Second)
	first, err := model.ClaimNativeRecovery(db, "same-worker", now, 1)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Equal(t, now.Add(2*time.Minute).Unix(), first[0].RecoveryUntil)
	require.NoError(t, model.ReleaseNativeRecovery(db, "same", first[0].RecoveryOwner, now))
	second, err := model.ClaimNativeRecovery(db, "same-worker", now, 1)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Equal(t, first[0].RecoveryUntil, second[0].RecoveryUntil, "legitimate early release/reclaim in one second")
	require.NotEqual(t, first[0].RecoveryOwner, second[0].RecoveryOwner, "timestamps and worker label are not a generation")
	released, err := model.ReleaseNativeRecoveryAt(db, "same", first[0].RecoveryOwner, "reserved", now.Add(time.Hour))
	require.NoError(t, err)
	require.False(t, released)
	require.Error(t, model.ReleaseNativeRecovery(db, "same", first[0].RecoveryOwner, now.Add(time.Hour)))
	current, err := model.GetNativeTask(db, "same", "g", 1)
	require.NoError(t, err)
	require.Equal(t, second[0].RecoveryOwner, current.RecoveryOwner)
	require.Equal(t, second[0].RecoveryUntil, current.RecoveryUntil)
	require.NoError(t, model.ReleaseNativeRecovery(db, "same", second[0].RecoveryOwner, now))
}

func TestNativeRecoveryExtensionExpiryDuringWriteRollsBack(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "extension.db"))
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	defer pool.Close()
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	_, _, err = model.ReserveNativeTask(db, model.NativeTask{ID: "extend", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "f", OutputSchema: `{}`})
	require.NoError(t, err)
	claims, err := model.ClaimNativeRecovery(db, "worker", time.Now(), 1)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	claim := claims[0]
	deadline := time.Now().Add(2 * time.Second).Truncate(time.Second)
	require.NoError(t, db.Model(&model.NativeTask{}).Where("id = ?", claim.ID).Update("recovery_until", deadline.Unix()).Error)
	require.NoError(t, db.Callback().Update().After("gorm:update").Register("test:expire-extension", func(tx *gorm.DB) {
		changes, ok := tx.Statement.Dest.(map[string]any)
		if ok && tx.Statement.Table == "native_tasks" && changes["recovery_until"] != nil {
			if delay := time.Until(deadline.Add(40 * time.Millisecond)); delay > 0 {
				time.Sleep(delay)
			}
		}
	}))
	defer db.Callback().Update().Remove("test:expire-extension")
	require.Error(t, model.ExtendNativeRecovery(db, claim.ID, claim.RecoveryOwner, time.Now().Add(11*time.Minute)))
	current, err := model.GetNativeTask(db, claim.ID, "g", 1)
	require.NoError(t, err)
	require.Equal(t, deadline.Unix(), current.RecoveryUntil)
}
