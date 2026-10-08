package model_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// acceptedImageTask reserves and accepts an image task, then moves its
// acceptance time age into the past.
func acceptedImageTask(t *testing.T, id string, age time.Duration) {
	t.Helper()
	_, _, err := model.ReserveImageTask(&model.ImageTask{ID: id, GroupID: "g", TokenID: 1}, &model.AsyncUsageInfo{RequestID: id, GroupID: "g", TokenID: 1})
	require.NoError(t, err)
	require.NoError(t, model.AcceptImageTask(id, "upstream-"+id))
	require.NoError(t, model.LogDB.Model(&model.ImageTask{}).Where("id = ?", id).Update("queued_at", time.Now().UTC().Add(-age)).Error)
}

func setupDeadlineDB(t *testing.T) {
	t.Helper()
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "deadline.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))
	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })
}

// The store itself enforces the image rule: only an accepted task still
// waiting for the provider, AsyncGenerationDeadline after acceptance, expires;
// the task, its accounting outbox and its log row fail together, once.
func TestExpireImageGenerationIsCompareAndSet(t *testing.T) {
	setupDeadlineDB(t)
	now := time.Now()

	acceptedImageTask(t, "young", model.AsyncGenerationDeadline-time.Minute)
	acceptedImageTask(t, "queued", model.AsyncGenerationDeadline+time.Second)
	acceptedImageTask(t, "running", time.Hour)
	require.NoError(t, model.SetImageTaskResult("running", "in_progress", nil, nil))
	acceptedImageTask(t, "processing", time.Hour)
	require.NoError(t, model.SetImageTaskResult("processing", "result_processing", []model.ImageOutput{{URL: "https://source/image"}}, nil))
	acceptedImageTask(t, "done", time.Hour)
	require.NoError(t, model.SetImageTaskResult("done", "completed", []model.ImageOutput{{URL: "https://source/image"}}, nil))
	_, _, err := model.ReserveImageTask(&model.ImageTask{ID: "unknown", GroupID: "g", TokenID: 1}, &model.AsyncUsageInfo{RequestID: "unknown"})
	require.NoError(t, err)
	require.NoError(t, model.SetImageTaskResult("unknown", "submission_unknown", nil, nil))
	require.NoError(t, model.LogDB.Model(&model.ImageTask{}).Where("id = ?", "unknown").Update("created_at", now.UTC().Add(-time.Hour)).Error)

	for id, want := range map[string]bool{"young": false, "queued": true, "running": true, "processing": false, "done": false, "unknown": false} {
		expired, err := model.ExpireImageGeneration(id, now)
		require.NoError(t, err)
		require.Equal(t, want, expired, id)
	}
	again, err := model.ExpireImageGeneration("running", now)
	require.NoError(t, err)
	require.False(t, again, "a second call never re-fails")

	for id, status := range map[string]string{"young": "queued", "processing": "result_processing", "done": "completed", "unknown": "submission_unknown"} {
		task, err := model.GetImageTask(id, "g", 1)
		require.NoError(t, err)
		require.Equal(t, status, task.Status, id)
		require.Nil(t, task.Error, id)
	}

	task, err := model.GetImageTask("running", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "failed", task.Status)
	require.Equal(t, &model.ImageTaskError{Code: model.AsyncGenerationTimeoutCode, Message: model.AsyncGenerationTimeoutMessage}, task.Error)
	require.NotNil(t, task.CompletedAt)
	require.NotNil(t, task.RunningAt, "phase history is kept")

	var usage model.AsyncUsageInfo
	require.NoError(t, model.LogDB.First(&usage, "image_task_id = ?", "running").Error)
	require.Equal(t, model.AsyncUsageStatusFailed, usage.Status)
	var entry model.Log
	require.NoError(t, model.LogDB.First(&entry, usage.LogID).Error)
	require.Equal(t, 502, entry.Code)
	require.Equal(t, model.AsyncGenerationTimeoutCode, entry.ErrorCode)
	require.Equal(t, model.AsyncUsageStatusFailed, entry.AsyncUsageStatus)
	require.Equal(t, "Image generation timed out", entry.SafeError)
	require.EqualValues(t, "upstream-running", entry.UpstreamID)

	// A result arriving after expiry is discarded: failed is terminal.
	require.NoError(t, model.SetImageTaskResult("running", "completed", []model.ImageOutput{{URL: "https://late/image"}}, nil))
	task, err = model.GetImageTask("running", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "failed", task.Status)
	require.Empty(t, task.Data)
}

// The deadline counts from acceptance (queued_at), not from the reservation.
func TestImageGenerationDeadlineCountsFromAcceptance(t *testing.T) {
	now := time.Now()
	accepted := now.Add(-model.AsyncGenerationDeadline + time.Minute)
	task := &model.ImageTask{Status: "in_progress", UpstreamID: "u", CreatedAt: now.Add(-time.Hour), QueuedAt: &accepted}
	require.False(t, model.ImageGenerationExpired(task, now))
	require.True(t, model.ImageGenerationExpired(task, now.Add(time.Minute)))
	require.True(t, model.ImageGenerationDeadline(task).Equal(accepted.Add(model.AsyncGenerationDeadline)))
	task.Status = "result_processing"
	require.False(t, model.ImageGenerationExpired(task, now.Add(time.Hour)))
	task.Status, task.UpstreamID = "queued", ""
	require.False(t, model.ImageGenerationExpired(task, now.Add(time.Hour)))
}
