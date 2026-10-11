package model

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/config"
	"github.com/stretchr/testify/require"
)

// Retry log ids and request log ids are separate sequences that collide. The
// startup cleanup of expired retry logs must delete retry_logs rows only; the
// request log keeps its own retention (permanent when LOG_STORAGE_HOURS is 0).
func TestPreMigrationCleanupRetryLogsOnlyDeletesExpiredRetryLogs(t *testing.T) {
	t.Setenv("LOG_STORAGE_HOURS", "")
	t.Setenv("RETRY_LOG_STORAGE_HOURS", "")

	previousLogHours := config.GetLogStorageHours()
	previousRetryHours := config.GetRetryLogStorageHours()
	t.Cleanup(func() {
		config.SetLogStorageHours(previousLogHours)
		config.SetRetryLogStorageHours(previousRetryHours)
	})

	cases := []struct {
		name         string
		logHours     int64
		retryHours   int64
		keptRetryIDs []int
	}{
		{name: "retry retention", retryHours: 24, keptRetryIDs: []int{3}},
		// Matches the runtime cleaner: retry logs fall back to the log retention.
		{name: "log retention fallback", logHours: 24, keptRetryIDs: []int{3}},
		{name: "no retention", keptRetryIDs: []int{1, 2, 3}},
	}

	for _, tc := range cases {
		for _, batchSize := range []int{1, 100} {
			t.Run(fmt.Sprintf("%s batch %d", tc.name, batchSize), func(t *testing.T) {
				database, err := OpenSQLite(filepath.Join(t.TempDir(), "retry-cleanup.db"))
				require.NoError(t, err)
				require.NoError(t, database.AutoMigrate(&Log{}, &RetryLog{}))

				underlying, err := database.DB()
				require.NoError(t, err)

				previousLogDB := LogDB
				LogDB = database
				t.Cleanup(func() {
					LogDB = previousLogDB

					require.NoError(t, underlying.Close())
				})

				config.SetLogStorageHours(tc.logHours)
				config.SetRetryLogStorageHours(tc.retryHours)

				expired := time.Now().Add(-48 * time.Hour)
				for id := 1; id <= 3; id++ {
					require.NoError(t, database.Create(&Log{
						ID:        id,
						RequestID: EmptyNullString(fmt.Sprintf("log-%d", id)),
						CreatedAt: expired,
						RequestAt: expired,
						Code:      200,
					}).Error)
				}

				retryCreatedAt := map[int]time.Time{1: expired, 2: expired, 3: time.Now()}
				for id, createdAt := range retryCreatedAt {
					require.NoError(t, database.Create(&RetryLog{
						ID:        id,
						RequestID: EmptyNullString(fmt.Sprintf("retry-%d", id)),
						CreatedAt: createdAt,
						RequestAt: createdAt,
						Code:      500,
					}).Error)
				}

				require.NoError(t, preMigrationCleanupRetryLogs(batchSize))

				var logIDs []int
				require.NoError(t, database.Model(&Log{}).Order("id").Pluck("id", &logIDs).Error)
				require.Equal(t, []int{1, 2, 3}, logIDs, "request logs are never retry logs")

				var retryIDs []int
				require.NoError(
					t,
					database.Model(&RetryLog{}).Order("id").Pluck("id", &retryIDs).Error,
				)
				require.Equal(t, tc.keptRetryIDs, retryIDs)
			})
		}
	}
}

// The startup cleanup keeps the same rows as the runtime cleanLog: an expired
// request log that unsettled image accounting still points at survives.
func TestPreMigrationCleanupLogsKeepsUnsettledImageAccountingRows(t *testing.T) {
	t.Setenv("LOG_STORAGE_HOURS", "")

	previousLogHours := config.GetLogStorageHours()
	t.Cleanup(func() { config.SetLogStorageHours(previousLogHours) })

	database, err := OpenSQLite(filepath.Join(t.TempDir(), "log-cleanup.db"))
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&Log{}, &ImageTask{}, &AsyncUsageInfo{}))

	underlying, err := database.DB()
	require.NoError(t, err)

	previousLogDB := LogDB
	LogDB = database
	t.Cleanup(func() {
		LogDB = previousLogDB

		require.NoError(t, underlying.Close())
	})

	config.SetLogStorageHours(24)

	expired := time.Now().Add(-48 * time.Hour)
	for id := 1; id <= 3; id++ {
		require.NoError(t, database.Create(&Log{
			ID:        id,
			RequestID: EmptyNullString(fmt.Sprintf("log-%d", id)),
			CreatedAt: expired,
			RequestAt: expired,
			Code:      202,
		}).Error)
	}
	// Row 1: an image task still settling. Row 2: one already settled.
	require.NoError(t, database.Create(&AsyncUsageInfo{
		LogID: 1, ImageTaskID: "img-1", Status: AsyncUsageStatusPending,
	}).Error)
	require.NoError(t, database.Create(&AsyncUsageInfo{
		LogID: 2, ImageTaskID: "img-2", Status: AsyncUsageStatusCompleted,
	}).Error)

	require.NoError(t, preMigrationCleanupLogs(1))

	var kept []int
	require.NoError(t, database.Model(&Log{}).Order("id").Pluck("id", &kept).Error)
	require.Equal(t, []int{1}, kept)
}
