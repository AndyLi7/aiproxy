package model_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// enqueuedNativeTask is a reserved task moved into the own task queue.
func enqueuedNativeTask(t *testing.T, db *gorm.DB, id string) {
	t.Helper()

	_, created, err := model.ReserveNativeTask(db, model.NativeTask{
		ID:                 id,
		GroupID:            "g",
		TokenID:            1,
		Model:              "a/b/c",
		Fingerprint:        "hash",
		OutputSchema:       nativeLogSchema,
		BillingOperationID: "native:" + id,
		QueueScope:         "fal:default",
	})
	require.NoError(t, err)
	require.True(t, created)

	now := time.Now()
	require.NoError(
		t,
		model.EnqueueNativeTask(db, id, "g", 1, "fal:default", now, now.Add(5*time.Minute)),
	)
}

// Spec 2026-10-15 §6.5: a queued task whose hold the wallet refunded (its
// 2-hour backstop) can no longer be claimed and reads queue_timeout, not a
// provider that did not confirm.
func TestRefundedQueuedTaskFailsAsQueueTimeout(t *testing.T) {
	usageAt := time.Date(2026, time.October, 15, 1, 2, 3, 0, time.UTC)
	for _, claimed := range []bool{false, true} {
		db := nativeLogDB(t)
		require.NoError(t, db.AutoMigrate(&model.ImageTask{}))
		enqueuedNativeTask(t, db, "refunded")

		if claimed {
			task, _, err := model.ClaimNativeDispatch(db, model.NativeDispatchClaim{
				Scope: "fal:default", Owner: "o", Now: time.Now(),
				Pick: func(s *model.NativeQueueSnapshot) string { return s.Waiting[0].ID },
			})
			require.NoError(t, err)
			require.NotNil(t, task)
		}

		require.NoError(
			t,
			model.SaveNativeBillingTerminal(
				db,
				"refunded",
				"g",
				1,
				nativeReceipt("refunded", usageAt, model.RequestSourceAPI),
			),
		)
		task, err := model.GetNativeTask(db, "refunded", "g", 1)
		require.NoError(t, err)
		require.Equal(t, "failed", task.Status)
		require.Equal(t, model.QueueTimeoutCode, task.ErrorCode)
		require.True(t, task.BillingSettled)
		// It was answered 202 when it was queued: its lost call row is restored.
		require.Len(t, nativeLogRows(t, db, "refunded"), 1)
	}
}

// §7.1: a queued task recovery failed as not_started was answered 202 when it
// was enqueued, so a lost call row is inserted (unlike a reservation the
// stop-loss failed, which never got a 202).
func TestEnqueuedNotStartedTaskKeepsItsCallRow(t *testing.T) {
	usageAt := time.Date(2026, time.October, 15, 1, 2, 3, 0, time.UTC)
	db := nativeLogDB(t)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))
	enqueuedNativeTask(t, db, "lease_ran_out")
	_, _, err := model.ClaimNativeDispatch(db, model.NativeDispatchClaim{
		Scope: "fal:default", Owner: "o", Now: time.Now(),
		Pick: func(s *model.NativeQueueSnapshot) string { return s.Waiting[0].ID },
	})
	require.NoError(t, err)
	expired, err := model.ExpireNativeDispatch(
		db,
		"lease_ran_out",
		"g",
		1,
		"o",
		time.Now().Add(model.NativeDispatchLease+time.Second),
	)
	require.NoError(t, err)
	require.True(t, expired)
	require.NoError(
		t,
		model.SaveNativeBillingTerminal(
			db,
			"lease_ran_out",
			"g",
			1,
			nativeReceipt("refunded", usageAt, model.RequestSourceAPI),
		),
	)
	rows := nativeLogRows(t, db, "lease_ran_out")
	require.Len(t, rows, 1)
	require.Equal(t, model.NotStartedCode, rows[0].ErrorCode)
	require.Equal(t, 0.0, rows[0].Amount.UsedAmount)
}

// §5.3: in flight counts dispatching..running native tasks of the scope and
// the provider's image tasks; finished results do not count.
func TestQueueSnapshotCountsInFlightWork(t *testing.T) {
	db := nativeLogDB(t)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))

	for id, status := range map[string]string{"n1": "submitting", "n2": "queued", "n3": "running", "n4": "result_received", "n5": "completed"} {
		_, _, err := model.ReserveNativeTask(db, model.NativeTask{
			ID:                 id,
			GroupID:            "g" + id,
			TokenID:            1,
			Model:              "a/b/c",
			Fingerprint:        "hash",
			OutputSchema:       nativeLogSchema,
			BillingOperationID: "native:" + id,
			QueueScope:         "fal:default",
		})
		require.NoError(t, err)
		require.NoError(
			t,
			db.Model(&model.NativeTask{}).Where("id = ?", id).Update("status", status).Error,
		)
	}

	require.NoError(
		t,
		db.Create(
			&model.ImageTask{
				ID:          "i1",
				GroupID:     "gi",
				TokenID:     1,
				Status:      "in_progress",
				ChannelType: model.ChannelTypeFal,
			},
		).Error,
	)
	require.NoError(
		t,
		db.Create(
			&model.ImageTask{
				ID:          "i2",
				GroupID:     "gi",
				TokenID:     1,
				Status:      "completed",
				ChannelType: model.ChannelTypeFal,
			},
		).Error,
	)
	snapshot, err := model.ReadNativeQueueSnapshot(
		db,
		"fal:default",
		time.Now(),
		model.ImageTasksInFlight(model.ChannelTypeFal),
	)
	require.NoError(t, err)
	require.Equal(t, int64(4), snapshot.InFlight)
	require.Equal(
		t,
		map[string]int64{"gn1": 1, "gn2": 1, "gn3": 1, "gi": 1},
		snapshot.InFlightByGroup,
	)

	other, err := model.ReadNativeQueueSnapshot(db, "other:default", time.Now(), nil)
	require.NoError(t, err)
	require.Zero(t, other.InFlight)
}

// Review 16: a reserved task whose hold the wallet still had queued when it
// refunded it (the 2-hour backstop) reads as a queue timeout; a reserved
// task with a direct hold keeps submission_timeout.
func TestRefundedReservationWithAQueuedHoldIsAQueueTimeout(t *testing.T) {
	usageAt := time.Date(2026, time.October, 15, 1, 2, 3, 0, time.UTC)
	for submission, want := range map[string]string{"waiting": model.QueueTimeoutCode, "not_submitted": "submission_timeout"} {
		db := nativeLogDB(t)
		_, created, err := model.ReserveNativeTask(db, model.NativeTask{
			ID:                 "r",
			GroupID:            "g",
			TokenID:            1,
			Model:              "a/b/c",
			Fingerprint:        "hash",
			OutputSchema:       nativeLogSchema,
			BillingOperationID: "native:r",
			QueueScope:         "fal:default",
		})
		require.NoError(t, err)
		require.True(t, created)

		receipt := `{"id":"native:r","status":"refunded","chargedMicros":0,"refundMicros":200,"submissionStatus":"` + submission + `","usageAtMs":` +
			strconv.FormatInt(
				usageAt.UnixMilli(),
				10,
			) + `,"usageSource":"api"}`
		require.NoError(t, model.SaveNativeBillingTerminal(db, "r", "g", 1, receipt))
		task, err := model.GetNativeTask(db, "r", "g", 1)
		require.NoError(t, err)
		require.Equal(t, "failed", task.Status)
		require.Equal(t, want, task.ErrorCode, submission)
	}
}

// Review 12: a customer's active-task estimate reads one day: a completed
// task still waiting for its bill counts for a day, as in the wallet.
func TestActiveEstimateReadsOneDay(t *testing.T) {
	db := nativeLogDB(t)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))

	now := time.Now()
	for id, row := range map[string]struct {
		status string
		age    time.Duration
	}{"recent": {"completed", time.Hour}, "old": {"completed", 25 * time.Hour}, "running": {"running", time.Minute}} {
		_, _, err := model.ReserveNativeTask(db, model.NativeTask{
			ID:                 id,
			GroupID:            "g",
			TokenID:            1,
			Model:              "a/b/c",
			Fingerprint:        "hash",
			OutputSchema:       nativeLogSchema,
			BillingOperationID: "native:" + id,
			QueueScope:         "fal:default",
		})
		require.NoError(t, err)
		require.NoError(t, db.Model(&model.NativeTask{}).Where("id = ?", id).
			Updates(map[string]any{"status": row.status, "created_at": now.Add(-row.age)}).Error)
	}

	enqueuedNativeTask(t, db, "waiting")
	snapshot, err := model.ReadNativeQueueSnapshot(db, "fal:default", time.Now(), nil)
	require.NoError(t, err)
	require.Equal(
		t,
		int64(2),
		snapshot.ActiveByGroup["g"],
		"running and the recent unbilled one; not the day-old one",
	)
}
