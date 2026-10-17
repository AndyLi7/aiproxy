package task

import (
	"context"
	"github.com/labring/aiproxy/core/common/balance"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func backgroundWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("background work did not return")
	}
}
func backgroundStillRunning(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("returned before owned work/cleanup exited")
	case <-time.After(30 * time.Millisecond):
	}
}
func TestBackgroundAsyncJoinsPollAndBothChildren(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ticks := make(chan time.Time, 1)
	entered := make(chan struct{}, 3)
	release := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	for _, r := range release {
		defer close(r)
	}
	work := func(i int) func(context.Context) {
		return func(ctx context.Context) { entered <- struct{}{}; <-ctx.Done(); <-release[i] }
	}
	done := make(chan struct{})
	go func() {
		runAsyncUsagePollTask(ctx, ticks, func(ctx context.Context) bool { work(0)(ctx); return true }, work(1), work(2))
		close(done)
	}()
	ticks <- time.Now()
	for range 3 {
		backgroundWait(t, entered)
	}
	cancel()
	backgroundStillRunning(t, done)
	// Each owner must return, not just observe cancellation. Send a release per owner.
	release[0] <- struct{}{}
	backgroundStillRunning(t, done)
	release[1] <- struct{}{}
	backgroundStillRunning(t, done)
	release[2] <- struct{}{}
	backgroundWait(t, done)
}
func TestBackgroundNativeJoinsAllFourWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{}, nativeRecoveryWorkers)
	release := make(chan struct{})
	defer close(release)
	done := make(chan struct{})
	go func() {
		runNativeTaskRecovery(ctx, func(ctx context.Context) { entered <- struct{}{}; <-ctx.Done(); <-release })
		close(done)
	}()
	for range nativeRecoveryWorkers {
		backgroundWait(t, entered)
	}
	cancel()
	for range nativeRecoveryWorkers - 1 {
		release <- struct{}{}
		backgroundStillRunning(t, done)
	}
	release <- struct{}{}
	backgroundWait(t, done)
}
func TestBackgroundCancellationWinsReadyTickAndFullBatch(t *testing.T) {
	for range 100 {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		ticks := make(chan time.Time, 1)
		ticks <- time.Now()
		runRecoveryTicks(ctx, ticks, func(context.Context) { t.Fatal("cancelled loop started work") })
		runAsyncUsagePollTask(ctx, ticks, func(context.Context) bool { t.Fatal("cancelled poll"); return true }, func(context.Context) { t.Error("cancelled child") })
		runNativeTaskRecovery(ctx, func(context.Context) { t.Error("cancelled worker") })
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	var calls int
	runAsyncUsagePollTask(ctx, ticks, func(context.Context) bool { calls++; cancel(); return true })
	require.Equal(t, 1, calls)
}
func TestBackgroundCancelledActualTasksNeedNoDatabase(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	old := model.LogDB
	model.LogDB = nil
	defer func() { model.LogDB = old }()
	AsyncUsagePollTask(ctx)
	NativeTaskRecoveryTask(ctx)
	ImagePrepaymentRecoveryTask(ctx)
	UpstreamBillingRecoveryTask(ctx)
	require.False(t, processAsyncUsages(ctx))
	won, err := claimAsyncUsageContext(ctx, &model.AsyncUsageInfo{ID: 1})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, won)
}
func TestBackgroundAsyncCancelAfterScanDoesNotClaim(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "cancel.db"))
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	defer pool.Close()
	require.NoError(t, db.AutoMigrate(&model.AsyncUsageInfo{}, &model.ImageTask{}))
	old := model.LogDB
	model.LogDB = db
	defer func() { model.LogDB = old }()
	info := model.AsyncUsageInfo{Status: model.AsyncUsageStatusPending, NextPollAt: time.Now().Add(-time.Minute)}
	require.NoError(t, db.Create(&info).Error)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var scans atomic.Int32
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("cancel-after-scan", func(tx *gorm.DB) {
		if tx.Statement.Table == "async_usage_infos" {
			scans.Add(1)
			cancel()
		}
	}))
	require.False(t, processAsyncUsages(ctx))
	require.EqualValues(t, 1, scans.Load())
	require.NoError(t, db.Callback().Query().Remove("cancel-after-scan"))
	var current model.AsyncUsageInfo
	require.NoError(t, db.First(&current, info.ID).Error)
	require.Empty(t, current.ProcessingToken)
	require.True(t, current.NextPollAt.Equal(info.NextPollAt))
	// A live caller retains the previous claim semantics and three-minute lease.
	won, err := claimAsyncUsageContext(t.Context(), &current)
	require.NoError(t, err)
	require.True(t, won)
	require.NotEmpty(t, current.ProcessingToken)
	require.WithinDuration(t, time.Now().Add(asyncUsageProcessingLease), current.NextPollAt, time.Second)
}

func TestBackgroundBillingCancelsActualHTTPWithoutClaimingRemoteExit(t *testing.T) {
	entered, remoteRelease, remoteReturned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-remoteRelease // A remote worker need not stop just because its caller cancels.
		close(remoteReturned)
	}))
	defer server.Close()
	defer close(remoteRelease)
	old := balance.Default
	balance.Default = balance.NewExternalHTTP(server.URL, "synthetic-billing-key")
	defer func() { balance.Default = old }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	done := make(chan struct{})
	go func() { runUpstreamBillingRecovery(ctx, ticks); close(done) }()
	backgroundWait(t, entered)
	cancel()
	backgroundWait(t, done)
	select {
	case <-remoteReturned:
		t.Fatal("fixture remote work unexpectedly returned")
	default:
	}
}
