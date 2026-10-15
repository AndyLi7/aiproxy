package model_test

import (
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeRecoveryRefundClosesUnknownSubmission(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "native.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	task, _, err := model.ReserveNativeTask(db, model.NativeTask{ID: "req", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "f", OutputSchema: `{}`})
	require.NoError(t, err)
	require.NoError(t, model.TransitionNativeSubmission(db, task.ID, "g", 1, "reserved", "submitting", ""))
	require.NoError(t, model.TransitionNativeSubmission(db, task.ID, "g", 1, "submitting", "submission_unknown", "submission_outcome_unknown"))
	require.NoError(t, model.SaveNativeBillingTerminal(db, task.ID, "g", 1, `{"status":"refunded"}`))
	task, err = model.GetNativeTask(db, task.ID, "g", 1)
	require.NoError(t, err)
	require.Equal(t, "failed", task.Status)
	require.Equal(t, "submission_timeout", task.ErrorCode)
	claimed, err := model.ClaimNativeRecovery(db, "worker", time.Now(), 20)
	require.NoError(t, err)
	require.Empty(t, claimed)
}

func TestNativeRecoveryLeaseExtendsOnlyForItsOwnerAndNeverShortens(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "native.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	task, _, err := model.ReserveNativeTask(db, model.NativeTask{ID: "req", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "f", OutputSchema: `{}`})
	require.NoError(t, err)
	now := time.Now()
	claimed, err := model.ClaimNativeRecovery(db, "worker", now, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	until := func() int64 {
		row, err := model.GetNativeTask(db, task.ID, "g", 1)
		require.NoError(t, err)
		return row.RecoveryUntil
	}
	later := now.Add(11 * time.Minute)
	require.NoError(t, model.ExtendNativeRecovery(db, task.ID, "worker", later))
	require.Equal(t, later.Unix(), until())
	// An earlier end keeps the longer lease; it is not an error.
	require.NoError(t, model.ExtendNativeRecovery(db, task.ID, "worker", now.Add(time.Minute)))
	require.Equal(t, later.Unix(), until())
	// Another worker cannot extend, or take, a lease it does not hold.
	require.ErrorIs(t, model.ExtendNativeRecovery(db, task.ID, "other", now.Add(time.Hour)), model.ErrNativeTaskConflict)
	require.Equal(t, later.Unix(), until())
}

func TestExpiredRecoveryLeaseCannotBeExtended(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "native.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	_, _, err = model.ReserveNativeTask(db, model.NativeTask{ID: "req", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "f", OutputSchema: `{}`})
	require.NoError(t, err)
	require.NoError(t, db.Model(&model.NativeTask{}).Where("id = ?", "req").Updates(map[string]any{"recovery_owner": "old", "recovery_until": time.Now().Add(-time.Second).Unix()}).Error)
	require.ErrorIs(t, model.ExtendNativeRecovery(db, "req", "old", time.Now().Add(time.Minute)), model.ErrNativeTaskConflict)
}
