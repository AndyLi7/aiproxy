//nolint:testpackage // Uses the internal Postgres harness and the task lock.
package model

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func reserveSubmittingNativeTask(t *testing.T, db *gorm.DB, id string) *NativeTask {
	t.Helper()

	task, created, err := ReserveNativeTask(db, NativeTask{
		ID: id, GroupID: "g", TokenID: 1, Model: "vendor/model/text-to-image", Fingerprint: "f",
		OutputSchema: `{"type":"object"}`, BillingOperationID: "native:" + id,
	})
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, TransitionNativeSubmission(db, id, "g", 1, "reserved", "submitting", ""))

	return task
}

func ownNativeRows(t *testing.T, db *gorm.DB, id string) int64 {
	t.Helper()

	var count int64
	require.NoError(t, nativeTaskLogs(db, id, "g", 1).Count(&count).Error)

	return count
}

// Plan §1.4 rule 3: RecordNativeTaskLog and the recovery insert count and
// insert under the native task's row lock, so concurrent writers yield exactly
// one own row. Old: neither took a lock, and recovery never inserted.
func TestNativeLogWritersAreSerialized(t *testing.T) {
	withTestPostgresStoreDB(t, func() {
		require.NoError(t, LogDB.AutoMigrate(&NativeTask{}, &Log{}))

		usageAt := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
		receipt := fmt.Sprintf(`{"status":"pending","usageAtMs":%d,"usageSource":"api"}`, usageAt.UnixMilli())
		info := NativeTaskLog{RequestAt: usageAt, Mode: int(mode.NativeTasks), Endpoint: "POST /v1/model-tasks"}

		// A writer holding the lock with an uncommitted row makes the other wait,
		// then the other sees the row and adds nothing.
		t.Run("recovery waits for the submitting request", func(t *testing.T) {
			task := reserveSubmittingNativeTask(t, LogDB, "lock-record-first")
			tx := LogDB.Begin()
			_, err := lockNativeTask(tx, task.ID, "g", 1)
			require.NoError(t, err)
			require.NoError(t, tx.Create(&Log{
				RequestID: EmptyNullString(task.ID), GroupID: "g", TokenID: 1, Code: 202,
				Mode: int(mode.NativeTasks), AsyncUsageStatus: AsyncUsageStatusPending,
				BillingOperationID: EmptyNullString(task.BillingOperationID),
			}).Error)

			done := make(chan error, 1)
			go func() { done <- SaveNativeBillingTerminal(LogDB, task.ID, "g", 1, receipt) }()

			select {
			case err := <-done:
				_ = tx.Rollback()
				t.Fatalf("billing sync did not wait for the task lock: %v", err)
			case <-time.After(300 * time.Millisecond):
			}

			require.NoError(t, tx.Commit().Error)
			require.NoError(t, <-done)
			require.EqualValues(t, 1, ownNativeRows(t, LogDB, task.ID))
		})

		t.Run("the submitting request waits for recovery", func(t *testing.T) {
			task := reserveSubmittingNativeTask(t, LogDB, "lock-recovery-first")
			tx := LogDB.Begin()
			locked, err := lockNativeTask(tx, task.ID, "g", 1)
			require.NoError(t, err)
			parsed, ok := parseNativeReceiptUsage(receipt)
			require.True(t, ok)
			require.NoError(t, insertMissingNativeTaskLog(tx, locked, locked.Status, parsed, ""))

			done := make(chan error, 1)
			go func() { done <- RecordNativeTaskLog(LogDB, task, info) }()

			select {
			case err := <-done:
				_ = tx.Rollback()
				t.Fatalf("RecordNativeTaskLog did not wait for the task lock: %v", err)
			case <-time.After(300 * time.Millisecond):
			}

			require.NoError(t, tx.Commit().Error)
			require.NoError(t, <-done)
			require.EqualValues(t, 1, ownNativeRows(t, LogDB, task.ID))
		})

		t.Run("50 concurrent runs", func(t *testing.T) {
			for run := range 50 {
				task := reserveSubmittingNativeTask(t, LogDB, fmt.Sprintf("race-%d", run))
				start := make(chan struct{})
				errs := make(chan error, 2)

				var wg sync.WaitGroup

				wg.Add(2)

				go func() {
					defer wg.Done()
					<-start
					errs <- RecordNativeTaskLog(LogDB, task, info)
				}()
				go func() {
					defer wg.Done()
					<-start
					errs <- SaveNativeBillingTerminal(LogDB, task.ID, "g", 1, receipt)
				}()

				close(start)
				wg.Wait()
				close(errs)

				for err := range errs {
					require.NoError(t, err)
				}

				require.EqualValues(t, 1, ownNativeRows(t, LogDB, task.ID), "run %d", run)
			}
		})
	})
}

// S1/S2 (2026-10-13): recovery fails a reservation that was never submitted as
// not_started and refunds it. Its billing syncs read the task under the lock
// as failed, so the guard must read error_code there too, or the sync inserts
// a 202 call row for a request that never got one.
func TestAbandonedReservationGainsNoCallRow(t *testing.T) {
	withTestPostgresStoreDB(t, func() {
		require.NoError(t, LogDB.AutoMigrate(&NativeTask{}, &Log{}))

		usageAt := time.Date(2026, 10, 13, 11, 21, 0, 0, time.UTC)
		_, created, err := ReserveNativeTask(LogDB, NativeTask{
			ID: "abandoned", GroupID: "g", TokenID: 1, Model: "vendor/model/text-to-image", Fingerprint: "f",
			OutputSchema: `{"type":"object"}`, BillingOperationID: "native:abandoned",
		})
		require.NoError(t, err)
		require.True(t, created)

		// The compare-and-set only takes a reservation older than its bound.
		abandoned, err := AbandonNativeReservation(LogDB, "abandoned", "g", 1, time.Now().Add(-time.Hour))
		require.NoError(t, err)
		require.False(t, abandoned)
		abandoned, err = AbandonNativeReservation(LogDB, "abandoned", "g", 1, time.Now().Add(time.Minute))
		require.NoError(t, err)
		require.True(t, abandoned)

		for _, status := range []string{"pending", "refunded"} {
			receipt := fmt.Sprintf(`{"status":%q,"chargedMicros":0,"refundMicros":200,"usageAtMs":%d,"usageSource":"api"}`, status, usageAt.UnixMilli())
			require.NoError(t, SaveNativeBillingTerminal(LogDB, "abandoned", "g", 1, receipt))
		}

		require.Zero(t, ownNativeRows(t, LogDB, "abandoned"))
		task, err := GetNativeTask(LogDB, "abandoned", "g", 1)
		require.NoError(t, err)
		require.Equal(t, "failed", task.Status)
		require.Equal(t, NotStartedCode, task.ErrorCode)
		require.True(t, task.BillingSettled)

		// A submission that already left reserved is never abandoned.
		reserveSubmittingNativeTask(t, LogDB, "submitting")
		abandoned, err = AbandonNativeReservation(LogDB, "submitting", "g", 1, time.Now().Add(time.Minute))
		require.NoError(t, err)
		require.False(t, abandoned)
	})
}
