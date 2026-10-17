//nolint:testpackage // Exercise actual SQLite driver codes and transaction/cache boundaries.
package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqlitedriver "github.com/glebarez/go-sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// Obtain a real extended error, not a fabricated Code() implementation.
func statusBusySnapshotError(t *testing.T) error {
	t.Helper()
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "busy-error.db"))
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	require.NoError(t, db.Exec("PRAGMA journal_mode=WAL").Error)
	require.NoError(t, db.Exec("CREATE TABLE counter (id integer primary key, n integer)").Error)
	require.NoError(t, db.Exec("INSERT INTO counter VALUES(1,0)").Error)
	tx, err := pool.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	var n int
	require.NoError(t, tx.QueryRow("SELECT n FROM counter WHERE id=1").Scan(&n))
	require.NoError(t, db.Exec("UPDATE counter SET n=n+1 WHERE id=1").Error)
	_, err = tx.Exec("UPDATE counter SET n=n+1 WHERE id=1")
	var driverErr *sqlitedriver.Error
	require.ErrorAs(t, err, &driverErr)
	require.Equal(t, 517, driverErr.Code()) // SQLITE_BUSY_SNAPSHOT
	require.True(t, sqliteStatusBusy(fmt.Errorf("wrapped: %w", err)))
	require.False(t, sqliteStatusBusy(errors.New(err.Error())))
	return err
}

func TestSQLiteStatusRetriesWholeTransactionAndRebuildsOnce(t *testing.T) {
	busy := statusBusySnapshotError(t)
	db, ch := useCacheRebuildDB(t)
	require.NoError(t, db.Exec("CREATE TABLE effects (n integer)").Error)
	require.NoError(t, db.Exec("INSERT INTO effects VALUES(0)").Error)
	require.NoError(t, db.Exec("CREATE TRIGGER status_effect AFTER UPDATE OF status ON channels BEGIN UPDATE effects SET n=n+1; END").Error)
	before := LoadModelCaches().Version
	var attempts atomic.Int32
	// Real SQL and trigger run, then an injected genuine driver BUSY forces the
	// entire transaction to roll back. Neither effects nor cache may leak through.
	require.NoError(t, db.Callback().Update().After("gorm:update").Register("test:status_rollback", func(tx *gorm.DB) {
		if tx.Statement.Table == "channels" && attempts.Add(1) < 3 {
			tx.AddError(busy)
		}
	}))
	require.NoError(t, UpdateChannelStatusByIDContext(t.Context(), ch.ID, ChannelStatusDisabled))
	require.EqualValues(t, 3, attempts.Load())
	var effects int
	require.NoError(t, db.Raw("SELECT n FROM effects").Scan(&effects).Error)
	require.Equal(t, 1, effects)
	require.Equal(t, before+1, LoadModelCaches().Version)
	require.False(t, cachedChannelEnabled(LoadModelCaches(), ch.ID))
}

func TestSQLiteStatusRetryCancellationExhaustionAndNonBusy(t *testing.T) {
	busy := statusBusySnapshotError(t)
	for _, mode := range []string{"exhausted", "cancel-backoff", "cancel-before", "nonbusy", "notfound", "rollback-unconfirmed"} {
		t.Run(mode, func(t *testing.T) {
			db, ch := useCacheRebuildDB(t)
			before := LoadModelCaches().Version
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls atomic.Int32
			expected := busy
			if mode == "nonbusy" {
				expected = errors.New("database is locked (5) (SQLITE_BUSY)")
			}
			require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test:status_failure", func(tx *gorm.DB) {
				if tx.Statement.Table != "channels" {
					return
				}
				calls.Add(1)
				if mode == "notfound" {
					return
				}
				if mode == "rollback-unconfirmed" {
					require.NoError(t, tx.Rollback().Error)
				}
				tx.AddError(expected)
				if mode == "cancel-backoff" {
					cancel()
				}
			}))
			if mode == "cancel-before" {
				cancel()
			}
			id := ch.ID
			if mode == "notfound" {
				id += 999
			}
			err := UpdateChannelStatusByIDContext(ctx, id, ChannelStatusDisabled)
			require.Error(t, err)
			switch mode {
			case "exhausted":
				require.ErrorIs(t, err, busy)
				require.EqualValues(t, sqliteStatusAttempts, calls.Load())
			case "cancel-backoff":
				require.ErrorIs(t, err, context.Canceled)
				require.EqualValues(t, 1, calls.Load())
			case "cancel-before":
				require.ErrorIs(t, err, context.Canceled)
				require.Zero(t, calls.Load())
			default:
				require.EqualValues(t, 1, calls.Load())
			}
			require.Equal(t, before, LoadModelCaches().Version)
			var row Channel
			require.NoError(t, db.First(&row, ch.ID).Error)
			require.Equal(t, ChannelStatusEnabled, row.Status)
		})
	}
	// Even an actual SQLite BUSY from a non-SQLite dialect must not be retried.
	record := &queueSQLRecorder{}
	pool := sql.OpenDB(shadowMySQLConnector{record: record, present: 0})
	defer pool.Close()
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: pool, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	require.NoError(t, err)
	oldDB, oldLog := DB, LogDB
	DB, LogDB = db, db
	defer func() { DB, LogDB = oldDB, oldLog }()
	var calls atomic.Int32
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test:mysql_busy", func(tx *gorm.DB) { calls.Add(1); tx.AddError(busy) }))
	require.ErrorIs(t, UpdateChannelStatusByIDContext(t.Context(), 7, ChannelStatusDisabled), busy)
	require.EqualValues(t, 1, calls.Load())
}

func TestSQLiteStatusFreshGuardAfterActualSnapshotConflict(t *testing.T) {
	for _, mode := range []string{"key-removed", "extension-installed", "unrelated-write", "request-cancelled"} {
		t.Run(mode, func(t *testing.T) {
			db, ch := useCacheRebuildDB(t)
			require.NoError(t, db.Exec("PRAGMA journal_mode=WAL").Error)
			require.NoError(t, db.Model(&Channel{}).Where("id=?", ch.ID).Update("status", ChannelStatusDisabled).Error)
			require.NoError(t, InitModelConfigAndChannelCache())
			before := LoadModelCaches().Version
			reached, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var reads, updates atomic.Int32
			require.NoError(t, db.Callback().Query().After("gorm:query").Register("test:status_old_snapshot", func(tx *gorm.DB) {
				if tx.Statement.Table == "channels" {
					reads.Add(1)
					once.Do(func() { close(reached); <-release })
				}
			}))
			require.NoError(t, db.Callback().Update().After("gorm:update").Register("test:status_busy_observed", func(tx *gorm.DB) {
				if tx.Statement.Table == "channels" && sqliteStatusBusy(tx.Error) {
					updates.Add(1)
				}
			}))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- UpdateChannelStatusByIDContext(ctx, ch.ID, ChannelStatusEnabled) }()
			select {
			case <-reached:
			case <-time.After(3 * time.Second):
				t.Fatal("status did not reach old read snapshot")
			}
			switch mode {
			case "key-removed":
				require.NoError(t, db.Exec("UPDATE channels SET key='' WHERE id=?", ch.ID).Error)
			case "extension-installed":
				require.NoError(t, db.Exec("CREATE TABLE channel_migration_shadow_protocols(id integer)").Error)
			case "unrelated-write":
				require.NoError(t, db.Exec("UPDATE channels SET priority=priority+1 WHERE id=?", ch.ID).Error)
			case "request-cancelled":
				cancel()
			}
			close(release)
			err := <-done
			if mode == "unrelated-write" {
				require.NoError(t, err)
				require.GreaterOrEqual(t, reads.Load(), int32(2))
				require.Equal(t, before+1, LoadModelCaches().Version)
			} else {
				require.Error(t, err)
				require.Equal(t, before, LoadModelCaches().Version)
				if mode == "key-removed" {
					require.ErrorIs(t, err, ErrChannelKeyEmpty)
					require.GreaterOrEqual(t, reads.Load(), int32(2))
				}
				if mode == "extension-installed" {
					require.ErrorIs(t, err, ErrChannelAdmissionUnavailable)
				}
				if mode == "request-cancelled" {
					require.ErrorIs(t, err, context.Canceled)
				}
			}
			if mode != "request-cancelled" {
				require.GreaterOrEqual(t, updates.Load(), int32(1), "must exercise actual driver snapshot BUSY")
			}
			var row Channel
			require.NoError(t, db.First(&row, ch.ID).Error)
			if mode != "unrelated-write" {
				require.Equal(t, ChannelStatusDisabled, row.Status)
			}
		})
	}
}
