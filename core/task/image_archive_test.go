package task

import (
	"context"
	"errors"
	"github.com/labring/aiproxy/core/common/ownedimage"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"testing"
	"time"
)

func TestImageArchiveResumesWithoutRegeneration(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))
	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })
	task := &model.ImageTask{ID: "archive", Status: "result_processing", ArchiveRequired: true, ExpectedImages: 2, Data: []model.ImageOutput{{URL: "https://upstream/1"}, {URL: "https://upstream/2"}}}
	require.NoError(t, db.Create(task).Error)
	calls := 0
	err = archiveImageTask(context.Background(), task, func(_ context.Context, src string) (string, ownedimage.Metadata, error) {
		calls++
		if calls == 2 {
			return "", ownedimage.Metadata{}, errors.New("temporary")
		}
		return "https://owned/1", ownedimage.Metadata{Width: 1024, Height: 768, ContentType: "image/png"}, nil
	})
	require.Error(t, err)
	var saved model.ImageTask
	require.NoError(t, db.First(&saved, "id = ?", task.ID).Error)
	require.Equal(t, "result_processing", saved.Status)
	require.True(t, saved.Data[0].Stored)
	require.False(t, saved.Data[1].Stored)
	require.NoError(t, archiveImageTask(context.Background(), &saved, func(_ context.Context, src string) (string, ownedimage.Metadata, error) {
		require.Equal(t, "https://upstream/2", src)
		calls++
		return "https://owned/2", ownedimage.Metadata{Width: 512, Height: 512, ContentType: "image/png"}, nil
	}))
	require.Equal(t, 3, calls)
	require.NoError(t, db.First(&saved, "id = ?", task.ID).Error)
	require.Equal(t, "completed", saved.Status)
	require.NotNil(t, saved.CompletedAt)
	require.WithinDuration(t, saved.CompletedAt.Add(7*24*time.Hour), *saved.ResultExpiresAt, time.Second)
	require.WithinDuration(t, saved.CompletedAt.Add(30*24*time.Hour), *saved.RetainUntil, time.Second)
	var n int64
	require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Count(&n).Error)
	require.Zero(t, n)
}
