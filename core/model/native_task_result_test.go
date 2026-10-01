package model_test

import (
	"encoding/json"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"path/filepath"
	"testing"
)

func TestNativeTaskResultOwnershipReplayAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native.db")
	db, err := model.OpenSQLite(path)
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	task := model.NativeTask{ID: "native-1", GroupID: "a", TokenID: 1, Model: "test/native", Fingerprint: "hash", OutputSchema: `{"type":"object","required":["seed","results"],"properties":{"seed":{"type":"integer"},"results":{"type":"array"}}}`}
	_, created, err := model.ReserveNativeTask(db, task)
	require.NoError(t, err)
	require.True(t, created)
	_, created, err = model.ReserveNativeTask(db, task)
	require.NoError(t, err)
	require.False(t, created)
	other := task
	other.TokenID = 2
	_, _, err = model.ReserveNativeTask(db, other)
	require.ErrorIs(t, err, model.ErrNativeTaskConflict)
	other = task
	other.Fingerprint = "different"
	_, _, err = model.ReserveNativeTask(db, other)
	require.ErrorIs(t, err, model.ErrNativeTaskConflict)
	other = task
	other.OutputSchema = `{"type":"array"}`
	_, _, err = model.ReserveNativeTask(db, other)
	require.ErrorIs(t, err, model.ErrNativeTaskConflict)
	raw := []byte(`{"seed":9007199254740993,"results":[],"extra":{"svg":"<svg/>"}}`)
	require.ErrorIs(t, model.SaveNativeTaskResult(db, task.ID, "a", 2, raw), gorm.ErrRecordNotFound)
	require.Error(t, model.SaveNativeTaskResult(db, task.ID, "a", 1, []byte(`{}`)))
	require.ErrorIs(t, model.SaveNativeTaskResult(db, task.ID, "a", 1, raw), model.ErrNativeTaskConflict)
	require.NoError(t, model.AcceptNativeTask(db, task.ID, "a", 1, "provider-1"))
	require.NoError(t, model.AcceptNativeTask(db, task.ID, "a", 1, "provider-1"))
	require.ErrorIs(t, model.AcceptNativeTask(db, task.ID, "a", 1, "provider-2"), model.ErrNativeTaskConflict)
	require.NoError(t, model.SaveNativeTaskResult(db, task.ID, "a", 1, raw))
	require.NoError(t, model.SaveNativeTaskResult(db, task.ID, "a", 1, raw))
	require.ErrorIs(t, model.SaveNativeTaskResult(db, task.ID, "a", 1, []byte(`{"seed":2,"results":[]}`)), model.ErrNativeTaskConflict)
	reopened, err := model.OpenSQLite(path)
	require.NoError(t, err)
	saved, err := model.GetNativeTask(reopened, task.ID, "a", 1)
	require.NoError(t, err)
	require.Equal(t, string(raw), saved.NativeOutput)
	require.Equal(t, "result_received", saved.Status)
	// A database record must never be sent directly as the public response.
	response, err := json.Marshal(saved)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(response))
	var count int64
	require.NoError(t, db.Model(&model.NativeTask{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
}
