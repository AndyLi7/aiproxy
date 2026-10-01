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
