package task

import (
	"context"
	"path/filepath"
	"strings"
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

func TestExpiredGIFCleanupKeepsPermanentExamplesAndTask(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "gif-cleanup.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))
	previous := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = previous })
	expired := time.Now().Add(-time.Hour)
	task := model.ImageTask{ID: "gif-task", Status: "completed", ArchiveRequired: true, ResultExpiresAt: &expired, Data: []model.ImageOutput{{URL: "https://media.test/generated-results/images/gif-task/0.gif", ContentType: "image/gif", Stored: true}, {URL: "https://media.test/capability-demo-permanent.gif", ContentType: "image/gif", Stored: true}}}
	require.NoError(t, db.Create(&task).Error)
	calls := 0
	changed, err := cleanupExpiredImageTask(context.Background(), &task, func(_ context.Context, id string, index int, mime string) error {
		calls++
		require.Equal(t, "gif-task", id)
		require.Equal(t, 0, index)
		require.Equal(t, "image/gif", mime)
		return nil
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, 1, calls)
	var saved model.ImageTask
	require.NoError(t, db.First(&saved, "id = ?", task.ID).Error)
	require.Empty(t, saved.Data[0].URL)
	require.Equal(t, "https://media.test/capability-demo-permanent.gif", saved.Data[1].URL)
	require.Equal(t, "completed", saved.Status)
	changed, err = cleanupExpiredImageTask(context.Background(), &saved, func(context.Context, string, int, string) error { t.Fatal("repeat deletion"); return nil })
	require.NoError(t, err)
	require.False(t, changed)
}

func TestTemporaryImageAssetAcceptsSignedAndLegacyNamesOnly(t *testing.T) {
	signature := "0123456789abcdef0123456789abcdef"
	png := func(url string) model.ImageOutput {
		return model.ImageOutput{URL: url, Stored: true, ContentType: "image/png"}
	}
	require.True(t, temporaryImageAsset("task", 0, "", png("https://media.test/generated-results/images/task/0-"+signature+".png")))
	require.True(t, temporaryImageAsset("task", 0, "", png("https://media.test/generated-results/images/task/0.png")))
	require.True(t, temporaryImageAsset("task", 0, "mask_image", png("https://media.test/generated-results/images/task/0-mask_image-"+signature+".png")))
	for _, url := range []string{
		"https://media.test/generated-results/images/task/0-mask_image-" + signature + ".png",
		"https://media.test/generated-results/images/task/1-" + signature + ".png",
		"https://media.test/generated-results/images/other/0-" + signature + ".png",
		"https://media.test/generated-results/images/task/0-" + strings.ToUpper(signature) + ".png",
		"https://media.test/generated-results/images/task/0-" + signature[:31] + ".png",
		"https://media.test/generated-results/images/task/0-" + signature + ".jpg",
		"http://media.test/generated-results/images/task/0-" + signature + ".png",
	} {
		require.False(t, temporaryImageAsset("task", 0, "", png(url)), url)
	}
	unstored := png("https://media.test/generated-results/images/task/0-" + signature + ".png")
	unstored.Stored = false
	require.False(t, temporaryImageAsset("task", 0, "", unstored))
}
