package model_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestNativeClaimEnforcesWaitingAgeWithoutRecovery(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		age, timeout, clockBehind time.Duration
		legacy                    bool
	}{
		{name: "five minute ceiling", age: 6 * time.Minute, timeout: time.Hour},
		{name: "shortened runtime limit", age: 2 * time.Minute, timeout: time.Minute},
		{name: "legacy missing enqueue time", age: 6 * time.Minute, legacy: true},
		{name: "stalled worker clock", age: 6 * time.Minute, clockBehind: 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := nativeLogDB(t)
			require.NoError(t, db.AutoMigrate(&model.ImageTask{}))
			enqueuedNativeTask(t, db, "expired")

			stamp := time.Now().Add(-tc.age).UTC()

			fields := map[string]any{"enqueued_at": stamp, "created_at": stamp}
			if tc.legacy {
				fields["enqueued_at"] = nil
			}

			require.NoError(
				t,
				db.Model(&model.NativeTask{}).Where("id = ?", "expired").Updates(fields).Error,
			)
			claimed, _, err := model.ClaimNativeDispatch(db, model.NativeDispatchClaim{
				Scope:       "fal:default",
				Owner:       "worker",
				Now:         time.Now().Add(-tc.clockBehind),
				WaitTimeout: tc.timeout,
				Pick:        func(s *model.NativeQueueSnapshot) string { return s.Waiting[0].ID },
			})
			require.NoError(t, err)
			require.Nil(t, claimed)

			task, err := model.GetNativeTask(db, "expired", "g", 1)
			require.NoError(t, err)
			require.Equal(
				t,
				model.NativeStatusWaiting,
				task.Status,
				"claim denial must leave the existing refund path in charge",
			)
			require.Empty(t, task.DispatchOwner)

			expired, err := model.ExpireNativeQueueWait(
				db,
				"expired",
				"g",
				1,
				time.Now().Add(-time.Minute),
			)
			require.NoError(t, err)
			require.True(t, expired)
		})
	}
}

func TestNativeClaimRechecksAgeAtUpdateAfterWorkerPause(t *testing.T) {
	db := nativeLogDB(t)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))
	enqueuedNativeTask(t, db, "slow")

	reachedUpdate := false
	require.NoError(
		t,
		db.Callback().
			Update().
			Before("gorm:update").
			Register("test_pause_claim", func(tx *gorm.DB) {
				if tx.Statement.Table == "native_tasks" {
					reachedUpdate = true

					time.Sleep(400 * time.Millisecond)
				}
			}),
	)
	t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove("test_pause_claim")) })

	claimed, _, err := model.ClaimNativeDispatch(db, model.NativeDispatchClaim{
		Scope: "fal:default", Owner: "worker", Now: time.Now(), WaitTimeout: 300 * time.Millisecond,
		Pick: func(s *model.NativeQueueSnapshot) string { return s.Waiting[0].ID },
	})
	require.NoError(t, err)
	require.True(t, reachedUpdate, "exercise the pause after the client cutoff was sampled")
	require.Nil(t, claimed)

	task, err := model.GetNativeTask(db, "slow", "g", 1)
	require.NoError(t, err)
	require.Equal(t, model.NativeStatusWaiting, task.Status)
}

func TestNativeClaimSkipsExpiredFrontAndKeepsDispatchLeaseSeparate(t *testing.T) {
	db := nativeLogDB(t)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))
	enqueuedNativeTask(t, db, "expired")
	enqueuedNativeTask(t, db, "live")
	require.NoError(
		t,
		db.Model(&model.NativeTask{}).
			Where("id = ?", "expired").
			Update("enqueued_at", time.Now().Add(-6*time.Minute).UTC()).
			Error,
	)
	original, err := model.GetNativeTask(db, "live", "g", 1)
	require.NoError(t, err)
	claimed, _, err := model.ClaimNativeDispatch(db, model.NativeDispatchClaim{
		Scope:       "fal:default",
		Owner:       "worker",
		Now:         time.Now(),
		WaitTimeout: time.Minute,
		Pick:        func(s *model.NativeQueueSnapshot) string { require.Len(t, s.Waiting, 1); return s.Waiting[0].ID },
	})
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, "live", claimed.ID)
	require.WithinDuration(t, *original.EnqueuedAt, *claimed.EnqueuedAt, time.Microsecond)
	// The waiting minute has elapsed, but the independent dispatch lease is live.
	require.NoError(
		t,
		model.StartNativeDispatchSubmission(
			db,
			"live",
			"g",
			1,
			"worker",
			time.Now().Add(90*time.Second),
		),
	)
	task, err := model.GetNativeTask(db, "live", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "submitting", task.Status)
	require.WithinDuration(t, *original.EnqueuedAt, *task.EnqueuedAt, time.Microsecond)
}

func TestNativeDispatchRefusesExpiredLeaseWithStaleCallerClock(t *testing.T) {
	db := nativeLogDB(t)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))
	enqueuedNativeTask(t, db, "late")
	claimed, _, err := model.ClaimNativeDispatch(db, model.NativeDispatchClaim{
		Scope: "fal:default", Owner: "worker", Now: time.Now(),
		Pick: func(s *model.NativeQueueSnapshot) string { return s.Waiting[0].ID },
	})
	require.NoError(t, err)
	require.NotNil(t, claimed)

	oldNow := time.Now().Add(-time.Minute)
	require.NoError(
		t,
		db.Model(&model.NativeTask{}).
			Where("id = ?", "late").
			Update("dispatch_until", time.Now().Add(-time.Second).UTC()).
			Error,
	)
	require.ErrorIs(
		t,
		model.StartNativeDispatchSubmission(db, "late", "g", 1, "worker", oldNow),
		model.ErrNativeTaskConflict,
	)
	task, err := model.GetNativeTask(db, "late", "g", 1)
	require.NoError(t, err)
	require.Equal(t, model.NativeStatusDispatching, task.Status)
}

// A restart must not make an old wait eligible again, even if the restarted
// worker's application clock lags. A separate connection sees the same facts.
func TestNativeWaitAgeSurvivesDatabaseReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "age.db")
	db, err := model.OpenSQLite(path)
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}, &model.ImageTask{}))
	enqueuedNativeTask(t, db, "old")

	original := time.Now().Add(-10 * time.Minute).UTC()
	require.NoError(
		t,
		db.Model(&model.NativeTask{}).Where("id = ?", "old").Update("enqueued_at", original).Error,
	)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	reopened, err := model.OpenSQLite(path)
	require.NoError(t, err)
	second, err := model.OpenSQLite(path)
	require.NoError(t, err)

	for _, connection := range []*gorm.DB{reopened, second} {
		t.Cleanup(
			func() { pool, e := connection.DB(); require.NoError(t, e); require.NoError(t, pool.Close()) },
		)

		claimed, _, claimErr := model.ClaimNativeDispatch(connection, model.NativeDispatchClaim{
			Scope: "fal:default", Owner: "restarted", Now: time.Now().Add(-15 * time.Minute),
			Pick: func(s *model.NativeQueueSnapshot) string { return s.Waiting[0].ID },
		})
		require.NoError(t, claimErr)
		require.Nil(t, claimed)
	}

	expired, err := model.ExpireNativeQueueWait(
		second,
		"old",
		"g",
		1,
		time.Now().Add(-5*time.Minute),
	)
	require.NoError(t, err)
	require.True(t, expired)

	stored, err := model.GetNativeTask(reopened, "old", "g", 1)
	require.NoError(t, err)
	require.Equal(t, model.QueueTimeoutCode, stored.ErrorCode)
	require.WithinDuration(t, original, *stored.EnqueuedAt, time.Microsecond)
}
