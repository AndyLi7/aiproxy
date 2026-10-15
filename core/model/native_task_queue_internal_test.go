//nolint:testpackage // Uses the internal Postgres harness, options and the claim lock.
package model

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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
			start := make(chan struct{})
			for i := range 8 {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start // all claimers race at once
					// Each claimer picks a different task, so only the scope lock,
					// never the row's compare-and-set, can keep the limit.
					task, _, err := ClaimNativeDispatch(LogDB, NativeDispatchClaim{Scope: "fal:default", Limit: 2,
						Owner: "owner-" + string(rune('a'+i)), Now: time.Now(),
						Pick: func(s *NativeQueueSnapshot) string { return s.Waiting[i%len(s.Waiting)].ID }})
					require.NoError(t, err)
					if task != nil {
						mu.Lock()
						claimed++
						mu.Unlock()
					}
				}(i)
			}
			close(start)
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
		require.NoError(t, LogDB.Exec("DROP INDEX IF EXISTS idx_native_tasks_group_created").Error)
		require.NoError(t, LogDB.Exec("INSERT INTO native_tasks (id, group_id, token_id, model, fingerprint, output_schema, output_schema_hash, status, error_code) VALUES ('kept', 'g', 1, 'a/b/c', 'fp', '{}', 'h', 'failed', 'upstream_rejected')").Error)
		require.False(t, NativeTaskStorageReady(LogDB))
		script, err := os.ReadFile(filepath.Join("..", "migrations", "native-task-v3", "postgres.sql"))
		require.NoError(t, err)
		indexes, err := os.ReadFile(filepath.Join("..", "migrations", "native-task-v3", "postgres-indexes.sql"))
		require.NoError(t, err)
		// psql's way: the simple protocol runs the column script as one
		// transaction, and each index statement on its own (CONCURRENTLY
		// cannot run in a transaction block).
		sqlDB, err := LogDB.DB()
		require.NoError(t, err)
		conn, err := sqlDB.Conn(context.Background())
		require.NoError(t, err)
		defer conn.Close()
		exec := func(text string) error {
			return conn.Raw(func(driverConn any) error {
				_, execErr := driverConn.(*stdlib.Conn).Conn().PgConn().Exec(context.Background(), text).ReadAll()
				return execErr
			})
		}
		for range 2 {
			require.NoError(t, exec(string(script)))
			for _, statement := range strings.Split(string(indexes), ";") {
				if body := sqlBody(statement); body != "" {
					require.NoError(t, exec(body))
				}
			}
		}
		require.True(t, NativeTaskStorageReady(LogDB))
		task, err := GetNativeTask(LogDB, "kept", "g", 1)
		require.NoError(t, err)
		require.Equal(t, "", task.QueueScope)
		require.Equal(t, "upstream_rejected", task.ErrorCode)
		var valid int64
		require.NoError(t, LogDB.Raw(`SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
			WHERE c.relname IN ('idx_native_tasks_status_queue_scope', 'idx_native_tasks_group_created') AND i.indisvalid`).Scan(&valid).Error)
		require.Equal(t, int64(2), valid)
	})
}

// sqlBody is a script statement without its comment lines.
func sqlBody(statement string) string {
	lines := []string{}
	for _, line := range strings.Split(statement, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// Review 19: an unreadable stored option is logged once, not at every
// 5-second sync, and again only when it changes.
func TestUnreadableQueueOptionIsLoggedOnce(t *testing.T) {
	reportUnreadableOption(config.NativeQueueEnabledKey, "", nil) // forget earlier tests' values
	hook := logtest.NewGlobal()
	t.Cleanup(func() {
		hook.Reset()
		config.SetNativeQueueEnabled(false)
		reportUnreadableOption(config.NativeQueueEnabledKey, "false", nil)
	})
	load := lenientOptionLoaders[config.NativeQueueEnabledKey]
	errors := func() int {
		n := 0
		for _, entry := range hook.AllEntries() {
			if entry.Level == logrus.ErrorLevel {
				n++
			}
		}
		return n
	}
	for range 3 {
		load(config.NativeQueueEnabledKey, `"true"`)
	}
	require.Equal(t, 1, errors())
	load(config.NativeQueueEnabledKey, `yes`)
	require.Equal(t, 2, errors(), "a different bad value is logged")
	load(config.NativeQueueEnabledKey, `true`)
	load(config.NativeQueueEnabledKey, `yes`)
	require.Equal(t, 3, errors(), "bad again after a good value")
}

// Review 18: claims run READ COMMITTED on PostgreSQL even when the database
// default is stricter; SQLite keeps its single-writer transactions.
func TestClaimTransactionIsReadCommitted(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "claims.db"))
	require.NoError(t, err)
	require.Empty(t, claimTxOptions(db))
	withTestPostgresStoreDB(t, func() {
		require.NoError(t, LogDB.Exec("ALTER DATABASE aiproxy_test SET default_transaction_isolation TO 'serializable'").Error)
		sqlDB, err := LogDB.DB()
		require.NoError(t, err)
		sqlDB.SetMaxIdleConns(0) // new connections pick up the stricter default
		var plain, claim string
		require.NoError(t, LogDB.Transaction(func(tx *gorm.DB) error {
			return tx.Raw("SHOW transaction_isolation").Scan(&plain).Error
		}))
		require.Equal(t, "serializable", plain)
		require.NoError(t, LogDB.Transaction(func(tx *gorm.DB) error {
			return tx.Raw("SHOW transaction_isolation").Scan(&claim).Error
		}, claimTxOptions(LogDB)...))
		require.Equal(t, "read committed", claim)
	})
}

// Review 19: a queue longer than one read is reported once a minute per scope.
func TestTruncatedQueueWarningIsThrottled(t *testing.T) {
	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)
	truncationLogged.Delete("throttle:test")
	for range 5 {
		logTruncatedQueue("throttle:test")
	}
	require.Len(t, hook.AllEntries(), 1)
	truncationLogged.Store("throttle:test", time.Now().Add(-2*time.Minute))
	logTruncatedQueue("throttle:test")
	require.Len(t, hook.AllEntries(), 2)
}
