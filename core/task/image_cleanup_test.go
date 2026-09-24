package task

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestExpiredImageCleanupDeletesOnlyScopedMediaAndKeepsTask(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "image-cleanup.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))
	previous := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = previous })
	expired := time.Now().Add(-time.Hour)
	task := model.ImageTask{
		ID: "request-123", Status: "completed", ArchiveRequired: true,
		ResultExpiresAt: &expired,
		Data: []model.ImageOutput{
			{URL: "https://r2.example/uploads/generated-results/images/request-123/0.png", Stored: true, ContentType: "image/png"},
			{URL: "https://r2.example/uploads/capability-demo-old.jpg", Stored: true, ContentType: "image/jpeg"},
		},
	}
	require.NoError(t, db.Create(&task).Error)
	found, err := model.ListExpiredTemporaryImageTasks(time.Now(), "", 10)
	require.NoError(t, err)
	require.Len(t, found, 1)
	calls := 0
	changed, err := cleanupExpiredImageTask(context.Background(), &found[0], func(_ context.Context, taskID string, index int, contentType string) error {
		calls++
		require.Equal(t, "request-123", taskID)
		require.Equal(t, 0, index)
		require.Equal(t, "image/png", contentType)
		return nil
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, 1, calls)
	var saved model.ImageTask
	require.NoError(t, db.First(&saved, "id = ?", task.ID).Error)
	require.Equal(t, "completed", saved.Status)
	require.Equal(t, "", saved.Data[0].URL)
	require.False(t, saved.Data[0].Stored)
	require.Equal(t, task.Data[1].URL, saved.Data[1].URL)
	found, err = model.ListExpiredTemporaryImageTasks(time.Now(), "", 10)
	require.NoError(t, err)
	require.Empty(t, found)
}
