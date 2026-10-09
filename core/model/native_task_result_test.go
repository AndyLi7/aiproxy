package model_test

import (
	"encoding/json"
	"github.com/labring/aiproxy/core/common/nativeresult"
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

func acceptedNativeTask(t *testing.T, db *gorm.DB, id string) {
	t.Helper()
	_, _, err := model.ReserveNativeTask(db, model.NativeTask{ID: id, GroupID: "g", TokenID: 1, Model: "a/b/c", Fingerprint: "hash", OutputSchema: `{}`})
	require.NoError(t, err)
	require.NoError(t, model.TransitionNativeSubmission(db, id, "g", 1, "reserved", "submitting", ""))
	require.NoError(t, model.AcceptNativeTask(db, id, "g", 1, "upstream-"+id))
}

// Owner decision 2026-10-09: invalid_parameters is an allowed failure on
// every path, always with the issues it names and never without them.
func TestNativeInvalidParametersIsStoredWithItsIssues(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "native.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	voice := []nativeresult.ParameterIssue{{Field: "voice", Rule: "unsupported_value"}}
	nine := make([]nativeresult.ParameterIssue, 9)
	for i := range nine {
		nine[i] = nativeresult.ParameterIssue{Field: "voice", Rule: "invalid"}
	}

	// Submit-time rejection.
	_, _, err = model.ReserveNativeTask(db, model.NativeTask{ID: "submit", GroupID: "g", TokenID: 1, Model: "a/b/c", Fingerprint: "hash", OutputSchema: `{}`})
	require.NoError(t, err)
	require.ErrorIs(t, model.TransitionNativeSubmission(db, "submit", "g", 1, "reserved", "submitting", "invalid_parameters", voice...), model.ErrNativeTaskConflict)
	require.NoError(t, model.TransitionNativeSubmission(db, "submit", "g", 1, "reserved", "submitting", ""))
	for _, bad := range [][]nativeresult.ParameterIssue{nil, nine, {{Field: "https://fal.ai", Rule: "invalid"}}, {{Field: "voice", Rule: "Voice not found"}}} {
		require.ErrorIs(t, model.TransitionNativeSubmission(db, "submit", "g", 1, "submitting", "failed", "invalid_parameters", bad...), model.ErrNativeTaskConflict)
	}
	require.ErrorIs(t, model.TransitionNativeSubmission(db, "submit", "g", 1, "submitting", "failed", "upstream_rejected", voice...), model.ErrNativeTaskConflict)
	require.NoError(t, model.TransitionNativeSubmission(db, "submit", "g", 1, "submitting", "failed", "invalid_parameters", voice...))
	task, err := model.GetNativeTask(db, "submit", "g", 1)
	require.NoError(t, err)
	require.Equal(t, voice, model.NativeTaskIssues(task))

	// Rejection after acceptance, through polling.
	acceptedNativeTask(t, db, "poll")
	require.ErrorIs(t, model.UpdateNativePoll(db, "poll", "g", 1, "failed", "invalid_parameters"), model.ErrNativeTaskConflict)
	require.ErrorIs(t, model.UpdateNativePoll(db, "poll", "g", 1, "failed", "upstream_task_failed", voice...), model.ErrNativeTaskConflict)
	require.ErrorIs(t, model.UpdateNativePoll(db, "poll", "g", 1, "failed", "something_new"), model.ErrNativeTaskConflict)
	require.NoError(t, model.UpdateNativePoll(db, "poll", "g", 1, "failed", "invalid_parameters", voice...))
	task, err = model.GetNativeTask(db, "poll", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "failed", task.Status)
	require.Equal(t, "invalid_parameters", task.ErrorCode)
	require.Equal(t, voice, model.NativeTaskIssues(task))

	// FailAcceptedNativeTask accepts it too, and other codes leave public_error empty.
	acceptedNativeTask(t, db, "fail")
	require.ErrorIs(t, model.FailAcceptedNativeTask(db, "fail", "g", 1, "invalid_parameters"), model.ErrNativeTaskConflict)
	require.NoError(t, model.FailAcceptedNativeTask(db, "fail", "g", 1, "invalid_parameters", voice...))
	task, err = model.GetNativeTask(db, "fail", "g", 1)
	require.NoError(t, err)
	require.Equal(t, voice, model.NativeTaskIssues(task))
	acceptedNativeTask(t, db, "other")
	require.NoError(t, model.FailAcceptedNativeTask(db, "other", "g", 1, "upstream_result_rejected"))
	task, err = model.GetNativeTask(db, "other", "g", 1)
	require.NoError(t, err)
	require.Empty(t, task.PublicError)
	require.Nil(t, model.NativeTaskIssues(task))
}
