package model_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestImageReservationReplayAndOwnership(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "image.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	task := &model.ImageTask{
		ID:          "req-a",
		GroupID:     "group",
		TokenID:     1,
		Model:       "public",
		Fingerprint: "hash",
	}
	saved, created, err := model.ReserveImageTask(task, &model.AsyncUsageInfo{RequestID: task.ID})
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "submitting", saved.Status)

	_, created, err = model.ReserveImageTask(task, &model.AsyncUsageInfo{})
	require.NoError(t, err)
	require.False(t, created)

	conflict := *task
	conflict.Fingerprint = "different"
	_, _, err = model.ReserveImageTask(&conflict, &model.AsyncUsageInfo{})
	require.ErrorIs(t, err, model.ErrImageTaskConflict)
	_, err = model.GetImageTask("req-a", "group", 2)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)

	var logEntry model.Log
	require.NoError(t, db.First(&logEntry).Error)
	require.Equal(t, model.EmptyNullString(task.ID), logEntry.RequestID)

	var count int64
	require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	require.NoError(t, model.AcceptImageTask(task.ID, "provider-id"))

	var info model.AsyncUsageInfo
	require.NoError(t, db.First(&info).Error)
	require.Equal(t, model.AsyncUsageStatusPending, info.Status)
	require.Equal(t, "provider-id", info.UpstreamID)
}

func TestImageReservationRollback(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "image.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	_, _, err = model.ReserveImageTask(&model.ImageTask{ID: "rollback"}, &model.AsyncUsageInfo{})
	require.Error(t, err)

	var count int64
	require.NoError(t, db.Model(&model.ImageTask{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestPendingImageTasksProtectChannelCredentials(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "channel.db"))
	require.NoError(t, err)
	require.NoError(
		t,
		db.AutoMigrate(
			&model.ImageTask{},
			&model.AsyncUsageInfo{},
			&model.Log{},
			&model.Channel{},
			&model.ChannelTest{},
		),
	)

	oldDB, oldLog := model.DB, model.LogDB
	model.DB, model.LogDB = db, db
	t.Cleanup(func() { model.DB, model.LogDB = oldDB, oldLog })

	ch := &model.Channel{Type: model.ChannelTypeFal, Key: "old"}
	require.NoError(t, db.Create(ch).Error)
	_, _, err = model.ReserveImageTask(
		&model.ImageTask{ID: "protected", GroupID: "g", TokenID: 1},
		&model.AsyncUsageInfo{ChannelID: ch.ID},
	)
	require.NoError(t, err)
	require.ErrorContains(t, model.DeleteChannelByID(ch.ID), "image tasks")
	require.ErrorContains(t, model.DeleteChannelsByIDs([]int{ch.ID}), "image tasks")

	replacement := "replacement"
	require.ErrorContains(
		t,
		model.UpdateChannel(ch, &model.ChannelPatch{Key: &replacement}),
		"image tasks",
	)
	current, err := model.GetChannelByID(ch.ID)
	require.NoError(t, err)
	require.Equal(t, "old", current.Key)
}

func TestImageReservationRejectsPreexistingOperationalRequestID(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "collision.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })
	require.NoError(t, db.Create(&model.Log{RequestID: "collision", Code: 404}).Error)
	_, _, err = model.ReserveImageTask(
		&model.ImageTask{ID: "collision"},
		&model.AsyncUsageInfo{RequestID: "collision"},
	)
	require.ErrorIs(t, err, model.ErrImageTaskConflict)

	var count int64
	require.NoError(t, db.Model(&model.ImageTask{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestImageAccountingLogRetention(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "retention.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	info := &model.AsyncUsageInfo{RequestID: "retained", GroupID: "g"}
	_, _, err = model.ReserveImageTask(&model.ImageTask{ID: "retained", GroupID: "g"}, info)
	require.NoError(t, err)
	deleted, err := model.DeleteOldLog(time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Zero(t, deleted)
	deleted, err = model.DeleteGroupLogs("g")
	require.NoError(t, err)
	require.Zero(t, deleted)
	require.NoError(t, db.Model(info).Update("status", model.AsyncUsageStatusCompleted).Error)

	deleted, err = model.DeleteOldLog(time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
}

func TestInterruptedImageSubmissionStaysReserved(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "unknown.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	task := &model.ImageTask{ID: "lost", GroupID: "g", TokenID: 1}
	_, _, err = model.ReserveImageTask(task, &model.AsyncUsageInfo{})
	require.NoError(t, err)
	require.NoError(t, db.Model(task).Update("updated_at", time.Now().Add(-3*time.Minute)).Error)
	require.NoError(t, model.RecoverStaleImageSubmissions(time.Now()))

	saved, err := model.GetImageTask("lost", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "submission_unknown", saved.Status)

	_, created, err := model.ReserveImageTask(task, &model.AsyncUsageInfo{})
	require.NoError(t, err)
	require.False(t, created)
}

func TestCompleteSyncImageTaskActivatesAccountingAtomically(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "sync.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))
	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })
	task, created, err := model.ReserveImageTask(&model.ImageTask{ID: "sync", GroupID: "g", TokenID: 1, Model: "image", Fingerprint: "x"}, &model.AsyncUsageInfo{RequestID: "sync"})
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, model.CompleteSyncImageTask(task.ID, []model.ImageOutput{{URL: "https://example.com/a.png"}}))
	got, err := model.GetImageTask(task.ID, "g", 1)
	require.NoError(t, err)
	require.Equal(t, "completed", got.Status)
	var info model.AsyncUsageInfo
	require.NoError(t, db.First(&info).Error)
	require.Equal(t, model.AsyncUsageStatusPending, info.Status)
	require.Error(t, model.CompleteSyncImageTask(task.ID, []model.ImageOutput{{URL: "https://example.com/b.png"}}))
	got, err = model.GetImageTask(task.ID, "g", 1)
	require.NoError(t, err)
	require.Equal(t, "https://example.com/a.png", got.Data[0].URL)
}
