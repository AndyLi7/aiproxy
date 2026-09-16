package model

import (
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"path/filepath"
	"testing"
	"time"
)

func TestImageReservationReplayAndOwnership(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "image.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&ImageTask{}, &AsyncUsageInfo{}, &Log{}))
	old := LogDB
	LogDB = db
	t.Cleanup(func() { LogDB = old })
	task := &ImageTask{ID: "req-a", GroupID: "group", TokenID: 1, Model: "public", Fingerprint: "hash"}
	saved, created, err := ReserveImageTask(task, &AsyncUsageInfo{RequestID: task.ID})
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "submitting", saved.Status)
	_, created, err = ReserveImageTask(task, &AsyncUsageInfo{})
	require.NoError(t, err)
	require.False(t, created)
	conflict := *task
	conflict.Fingerprint = "different"
	_, _, err = ReserveImageTask(&conflict, &AsyncUsageInfo{})
	require.ErrorIs(t, err, ErrImageTaskConflict)
	_, err = GetImageTask("req-a", "group", 2)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	var logEntry Log
	require.NoError(t, db.First(&logEntry).Error)
	require.Equal(t, EmptyNullString(task.ID), logEntry.RequestID)
	var count int64
	require.NoError(t, db.Model(&AsyncUsageInfo{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	require.NoError(t, AcceptImageTask(task.ID, "provider-id"))
	var info AsyncUsageInfo
	require.NoError(t, db.First(&info).Error)
	require.Equal(t, AsyncUsageStatusPending, info.Status)
	require.Equal(t, "provider-id", info.UpstreamID)
}
func TestImageReservationRollback(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "image.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&ImageTask{}))
	old := LogDB
	LogDB = db
	t.Cleanup(func() { LogDB = old })
	_, _, err = ReserveImageTask(&ImageTask{ID: "rollback"}, &AsyncUsageInfo{})
	require.Error(t, err)
	var count int64
	require.NoError(t, db.Model(&ImageTask{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestPendingImageTasksProtectChannelCredentials(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "channel.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&ImageTask{}, &AsyncUsageInfo{}, &Log{}, &Channel{}, &ChannelTest{}))
	oldDB, oldLog := DB, LogDB
	DB, LogDB = db, db
	t.Cleanup(func() { DB, LogDB = oldDB, oldLog })
	ch := &Channel{Type: ChannelTypeFal, Key: "old"}
	require.NoError(t, db.Create(ch).Error)
	_, _, err = ReserveImageTask(&ImageTask{ID: "protected", GroupID: "g", TokenID: 1}, &AsyncUsageInfo{ChannelID: ch.ID})
	require.NoError(t, err)
	require.ErrorContains(t, DeleteChannelByID(ch.ID), "image tasks")
	require.ErrorContains(t, DeleteChannelsByIDs([]int{ch.ID}), "image tasks")
	replacement := "replacement"
	require.ErrorContains(t, UpdateChannel(ch, &ChannelPatch{Key: &replacement}), "image tasks")
	current, err := GetChannelByID(ch.ID)
	require.NoError(t, err)
	require.Equal(t, "old", current.Key)
}

func TestImageReservationRejectsPreexistingOperationalRequestID(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "collision.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&ImageTask{}, &AsyncUsageInfo{}, &Log{}))
	old := LogDB
	LogDB = db
	t.Cleanup(func() { LogDB = old })
	require.NoError(t, db.Create(&Log{RequestID: "collision", Code: 404}).Error)
	_, _, err = ReserveImageTask(&ImageTask{ID: "collision"}, &AsyncUsageInfo{RequestID: "collision"})
	require.ErrorIs(t, err, ErrImageTaskConflict)
	var count int64
	require.NoError(t, db.Model(&ImageTask{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestImageAccountingLogRetention(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "retention.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&ImageTask{}, &AsyncUsageInfo{}, &Log{}))
	old := LogDB
	LogDB = db
	t.Cleanup(func() { LogDB = old })
	info := &AsyncUsageInfo{RequestID: "retained", GroupID: "g"}
	_, _, err = ReserveImageTask(&ImageTask{ID: "retained", GroupID: "g"}, info)
	require.NoError(t, err)
	deleted, err := DeleteOldLog(time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Zero(t, deleted)
	deleted, err = DeleteGroupLogs("g")
	require.NoError(t, err)
	require.Zero(t, deleted)
	require.NoError(t, db.Model(info).Update("status", AsyncUsageStatusCompleted).Error)
	deleted, err = DeleteOldLog(time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
}

func TestInterruptedImageSubmissionStaysReserved(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "unknown.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&ImageTask{}, &AsyncUsageInfo{}, &Log{}))
	old := LogDB
	LogDB = db
	t.Cleanup(func() { LogDB = old })
	task := &ImageTask{ID: "lost", GroupID: "g", TokenID: 1}
	_, _, err = ReserveImageTask(task, &AsyncUsageInfo{})
	require.NoError(t, err)
	require.NoError(t, db.Model(task).Update("updated_at", time.Now().Add(-3*time.Minute)).Error)
	require.NoError(t, RecoverStaleImageSubmissions(time.Now()))
	saved, err := GetImageTask("lost", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "submission_unknown", saved.Status)
	_, created, err := ReserveImageTask(task, &AsyncUsageInfo{})
	require.NoError(t, err)
	require.False(t, created)
}
