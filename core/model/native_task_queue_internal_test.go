//nolint:testpackage // Uses the internal Postgres harness, options and the claim lock.
package model

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/stretchr/testify/require"
)

// Spec 2026-10-15 §5.4, §10.1: claims from separate instances (no shared
// process lock) never exceed the scope's limit: PostgreSQL's transaction
// advisory lock serializes counting and claiming.
func TestPostgresConcurrentClaimsNeverExceedTheLimit(t *testing.T) {
	withTestPostgresStoreDB(t, func() {
		require.NoError(t, LogDB.AutoMigrate(&NativeTask{}, &ImageTask{}))
		claimLocally = false
		t.Cleanup(func() { claimLocally = true })
		for round := range 5 {
			require.NoError(t, LogDB.Exec("DELETE FROM native_tasks").Error)
			for i := range 8 {
				id := "task-" + string(rune('a'+i))
				_, _, err := ReserveNativeTask(LogDB, NativeTask{ID: id, GroupID: "g" + id, TokenID: 1, Model: "a/b/c",
					Fingerprint: "f", OutputSchema: `{"type":"object"}`, BillingOperationID: "native:" + id, QueueScope: "fal:default"})
				require.NoError(t, err)
				require.NoError(t, EnqueueNativeTask(LogDB, id, "g"+id, 1, "fal:default", time.Now(), time.Now().Add(time.Minute)))
			}
			var wg sync.WaitGroup
			var mu sync.Mutex
			claimed := 0
			for i := range 8 {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					task, _, err := ClaimNativeDispatch(LogDB, NativeDispatchClaim{Scope: "fal:default", Limit: 2,
						Owner: "owner-" + string(rune('a'+i)), Now: time.Now(),
						Pick: func(s *NativeQueueSnapshot) string { return s.Waiting[0].ID }})
					require.NoError(t, err)
					if task != nil {
						mu.Lock()
						claimed++
						mu.Unlock()
					}
				}(i)
			}
			wg.Wait()
			require.Equal(t, 2, claimed, "round %d", round)
			var dispatching int64
			require.NoError(t, LogDB.Model(&NativeTask{}).Where("status = ?", NativeStatusDispatching).Count(&dispatching).Error)
			require.Equal(t, int64(2), dispatching)
		}
	})
}

// §7.3.1: the queue options load leniently (a value this version cannot read
// falls back to its default and never stops the sync of other options) and
// save strictly.
func TestNativeQueueOptionsLoadLenientlyAndSaveStrictly(t *testing.T) {
	oldDB, oldKeys := DB, optionKeys
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "options.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Option{}))
	DB = db
	optionKeys = []string{config.NativeQueueEnabledKey, config.NativeQueueLimitsKey, config.NativeQueueTimeoutSecondsKey, "RetryTimes"}
	oldRetry := config.GetRetryTimes()
	t.Cleanup(func() {
		DB, optionKeys = oldDB, oldKeys
		config.SetNativeQueueEnabled(false)
		config.SetNativeQueueLimits(map[string]int{})
		config.SetNativeQueueTimeoutSeconds(int64(config.NativeQueueTimeout / time.Second))
		config.SetRetryTimes(oldRetry)
	})
	for key, value := range map[string]string{
		config.NativeQueueEnabledKey:        `"true"`,
		config.NativeQueueLimitsKey:         `{"fal:default":0}`,
		config.NativeQueueTimeoutSecondsKey: `900`,
		"RetryTimes":                        `7`,
	} {
		require.NoError(t, db.Create(&Option{Key: key, Value: value}).Error)
	}
	config.SetNativeQueueEnabled(true)
	config.SetNativeQueueLimits(map[string]int{"fal:default": 3})
	config.SetNativeQueueTimeoutSeconds(60)
	require.NoError(t, loadOptionsFromDatabase(false))
	require.False(t, config.GetNativeQueueEnabled())
	require.Empty(t, config.GetNativeQueueLimits())
	require.Equal(t, config.NativeQueueTimeout, config.GetNativeQueueTimeout())
	require.Equal(t, int64(7), config.GetRetryTimes(), "other options still load")
	require.NoError(t, loadOptionsFromDatabase(true))

	for key, bad := range map[string][]string{
		config.NativeQueueEnabledKey:        {`"true"`, `1`, `TRUE`, ``},
		config.NativeQueueLimitsKey:         {`{"fal:default":0}`, `{"fal:default":1.5}`, `{"fal":2}`, `[]`, `null`, `{"fal:default":2}{}`},
		config.NativeQueueTimeoutSecondsKey: {`0`, `301`, `abc`, `1.5`},
	} {
		for _, value := range bad {
			require.Error(t, UpdateOption(key, value), "%s=%s", key, value)
		}
	}
	require.NoError(t, UpdateOption(config.NativeQueueEnabledKey, "true"))
	require.NoError(t, UpdateOption(config.NativeQueueLimitsKey, `{"fal:default":8}`))
	require.NoError(t, UpdateOption(config.NativeQueueTimeoutSecondsKey, "120"))
	require.True(t, config.GetNativeQueueEnabled())
	require.Equal(t, map[string]int{"fal:default": 8}, config.GetNativeQueueLimits())
	require.Equal(t, 2*time.Minute, config.GetNativeQueueTimeout())
	saved, err := GetOption(config.NativeQueueLimitsKey)
	require.NoError(t, err)
	require.Equal(t, `{"fal:default":8}`, saved.Value)
}

// §8.1: the reviewed PostgreSQL script upgrades a table without the queue
// columns, keeps its rows, and is safe to run twice.
func TestPostgresNativeV3MigrationUpgradesAnEarlierTable(t *testing.T) {
	withTestPostgresStoreDB(t, func() {
		require.NoError(t, LogDB.AutoMigrate(&NativeTask{}))
		for _, column := range []string{"queue_scope", "enqueued_at", "dispatched_at", "dispatch_owner", "dispatch_until", "dispatch_after"} {
			require.NoError(t, LogDB.Exec("ALTER TABLE native_tasks DROP COLUMN "+column).Error)
		}
		require.NoError(t, LogDB.Exec("INSERT INTO native_tasks (id, group_id, token_id, model, fingerprint, output_schema, output_schema_hash, status, error_code) VALUES ('kept', 'g', 1, 'a/b/c', 'fp', '{}', 'h', 'failed', 'upstream_rejected')").Error)
		require.False(t, NativeTaskStorageReady(LogDB))
		script, err := os.ReadFile(filepath.Join("..", "migrations", "native-task-v3", "postgres.sql"))
		require.NoError(t, err)
		// psql's way: the simple protocol runs the whole script, transaction included.
		sqlDB, err := LogDB.DB()
		require.NoError(t, err)
		conn, err := sqlDB.Conn(context.Background())
		require.NoError(t, err)
		defer conn.Close()
		for range 2 {
			require.NoError(t, conn.Raw(func(driverConn any) error {
				_, execErr := driverConn.(*stdlib.Conn).Conn().PgConn().Exec(context.Background(), string(script)).ReadAll()
				return execErr
			}))
		}
		require.True(t, NativeTaskStorageReady(LogDB))
		task, err := GetNativeTask(LogDB, "kept", "g", 1)
		require.NoError(t, err)
		require.Equal(t, "", task.QueueScope)
		require.Equal(t, "upstream_rejected", task.ErrorCode)
		var indexes int64
		require.NoError(t, LogDB.Raw("SELECT count(*) FROM pg_indexes WHERE tablename = 'native_tasks' AND indexname = 'idx_native_tasks_status_queue_scope'").Scan(&indexes).Error)
		require.Equal(t, int64(1), indexes)
	})
}
