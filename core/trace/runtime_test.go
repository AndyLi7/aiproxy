package trace

import (
	"bytes"
	"context"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestStartDisabledDoesNotMigrateOrBecomeReady(t *testing.T) {
	db := openRuntimeSQLite(t)
	runtime := Start(context.Background(), db, Options{})
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })

	require.Equal(t, Health{}, runtime.Health())
	require.Nil(t, runtime.NewRequest("request-disabled"))
	require.False(t, db.Migrator().HasTable(&model.RequestTraceSpan{}))
}

func TestStartEnabledWithNilDatabaseStaysAvailableButNotReady(t *testing.T) {
	runtime := Start(context.Background(), nil, Options{Enabled: true})
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })

	require.Equal(t, Health{Enabled: true, InitializationFailed: true}, runtime.Health())
	require.Nil(t, runtime.NewRequest("request-no-db"))
}

func TestStartEnabledMigratesAndWritesNewRequests(t *testing.T) {
	db := openRuntimeSQLite(t)
	runtime := Start(context.Background(), db, Options{Enabled: true})
	require.True(t, runtime.Health().Ready)

	session := runtime.NewRequest("request-ready")
	require.NotNil(t, session)
	require.True(t, session.BindGroup("group-ready"))
	require.True(t, session.Finish(requesttrace.StatusSuccess))
	require.NoError(t, runtime.Close(context.Background()))

	var count int64
	require.Eventually(t, func() bool {
		return db.Model(&model.RequestTraceSpan{}).Count(&count).Error == nil && count == 1
	}, time.Second, 10*time.Millisecond)
}

func TestCleanupFailureIsCountedAndHealthIsConcurrentSafe(t *testing.T) {
	db := openRuntimeSQLite(t)
	ticks := make(chan time.Time, 1)
	oldTicker := newCleanupTicker
	newCleanupTicker = func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }
	t.Cleanup(func() { newCleanupTicker = oldTicker })

	runtime := Start(context.Background(), db, Options{Enabled: true})
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	ticks <- time.Now()
	require.Eventually(t, func() bool { return runtime.Health().CleanupErrors == 1 }, time.Second, 10*time.Millisecond)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = runtime.Health()
			}
		}()
	}
	wg.Wait()
	require.NoError(t, runtime.Close(context.Background()))
}

func TestRuntimeContextCancellationStopsCleanupWithoutClosingWriter(t *testing.T) {
	db := openRuntimeSQLite(t)
	ticks := make(chan time.Time, 1)
	stopped := make(chan struct{})
	oldTicker := newCleanupTicker
	newCleanupTicker = func(time.Duration) (<-chan time.Time, func()) {
		return ticks, func() { close(stopped) }
	}
	t.Cleanup(func() { newCleanupTicker = oldTicker })

	ctx, cancel := context.WithCancel(context.Background())
	runtime := Start(ctx, db, Options{Enabled: true})
	cancel()
	require.Eventually(t, func() bool {
		select {
		case <-stopped:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	session := runtime.NewRequest("request-after-cancel")
	require.NotNil(t, session)
	require.True(t, session.BindGroup("group-after-cancel"))
	require.True(t, session.Finish(requesttrace.StatusSuccess))
	require.NoError(t, runtime.Close(context.Background()))
}

func TestCloseHonorsCallerDeadline(t *testing.T) {
	db := openRuntimeSQLite(t)
	runtime := Start(context.Background(), db, Options{Enabled: true})
	db.Callback().Create().Before("gorm:create").Register("block_trace_write", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "RequestTraceHead" {
			<-tx.Statement.Context.Done()
		}
	})

	session := runtime.NewRequest("request-deadline")
	require.True(t, session.BindGroup("group-deadline"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, runtime.Close(ctx), context.DeadlineExceeded)
}

func TestCloseDeadlineStillClosesWriterWhenCleanupHasNotStopped(t *testing.T) {
	db := openRuntimeSQLite(t)
	releaseStop := make(chan struct{})
	tickerStarted := make(chan struct{})
	oldTicker := newCleanupTicker
	newCleanupTicker = func(time.Duration) (<-chan time.Time, func()) {
		close(tickerStarted)
		return make(chan time.Time), func() { <-releaseStop }
	}
	var releaseOnce sync.Once
	var runtime *Runtime
	release := func() {
		releaseOnce.Do(func() { close(releaseStop) })
		if runtime != nil && runtime.cleanupDone != nil {
			<-runtime.cleanupDone
		}
		newCleanupTicker = oldTicker
	}
	t.Cleanup(release)

	runtime = Start(context.Background(), db, Options{Enabled: true})
	<-tickerStarted
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, runtime.Close(ctx), context.DeadlineExceeded)

	before := runtime.Health().Writer.Rejected
	session := runtime.NewRequest("request-after-close-timeout")
	require.NotNil(t, session)
	require.True(t, session.BindGroup("group-after-close-timeout"))
	require.Greater(t, runtime.Health().Writer.Rejected, before)
	release()
}

func TestTraceDatabaseFailuresDoNotLeakThroughGORMLogger(t *testing.T) {
	var databaseLogs bytes.Buffer
	databaseLogger := logger.New(log.New(&databaseLogs, "", 0), logger.Config{LogLevel: logger.Warn})
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/trace.db"), &gorm.Config{Logger: databaseLogger})
	require.NoError(t, err)
	originalLogger := db.Logger

	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	runtime := Start(context.Background(), db, Options{Enabled: true})
	require.True(t, runtime.Health().InitializationFailed)
	require.Same(t, originalLogger, db.Logger)
	require.NotContains(t, databaseLogs.String(), "database is closed")
}

func TestCleanupDatabaseFailuresDoNotLeakThroughGORMLogger(t *testing.T) {
	var databaseLogs bytes.Buffer
	databaseLogger := logger.New(log.New(&databaseLogs, "", 0), logger.Config{LogLevel: logger.Warn})
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/trace.db"), &gorm.Config{Logger: databaseLogger})
	require.NoError(t, err)
	ticks := make(chan time.Time, 1)
	oldTicker := newCleanupTicker
	newCleanupTicker = func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }
	t.Cleanup(func() { newCleanupTicker = oldTicker })

	runtime := Start(context.Background(), db, Options{Enabled: true})
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	ticks <- time.Now()
	require.Eventually(t, func() bool { return runtime.Health().CleanupErrors == 1 }, time.Second, 10*time.Millisecond)
	require.NotContains(t, databaseLogs.String(), "database is closed")
	require.NoError(t, runtime.Close(context.Background()))
}

func TestInstallAndNilRuntimeAreSafe(t *testing.T) {
	restore := Install(nil)
	t.Cleanup(restore)
	require.Nil(t, Current())

	var runtime *Runtime
	require.Equal(t, Health{}, runtime.Health())
	require.Nil(t, runtime.NewRequest("request-nil"))
	require.NoError(t, runtime.Close(nil))
}

func openRuntimeSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/trace.db"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	return db
}
