//nolint:testpackage // White-box coverage needs the package-private startAsyncUsageClaimRenewalAtInterval fixture or implementation.
package task

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestAsyncUsageRenewalStopWaitsForOwnedCleanup(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "join.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.AsyncUsageInfo{}))
	pool, err := db.DB()
	require.NoError(t, err)

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old; require.NoError(t, pool.Close()) })

	info := &model.AsyncUsageInfo{
		RequestID:       "join",
		Status:          model.AsyncUsageStatusPending,
		ProcessingToken: "synthetic-token",
		NextPollAt:      time.Now().UTC().Add(time.Minute),
	}
	require.NoError(t, db.Create(info).Error)

	entered, release := make(chan struct{}), make(chan struct{})

	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()

	require.NoError(
		t,
		db.Callback().
			Update().
			After("gorm:update").
			Register("test:owned-cleanup", func(tx *gorm.DB) {
				if tx.Statement.Table == "async_usage_infos" {
					close(entered)
					<-release
				}
			}),
	)
	defer func() {
		if err := db.Callback().Update().Remove("test:owned-cleanup"); err != nil {
			t.Error(err)
		}
	}()

	ctx, stop := startAsyncUsageClaimRenewalAtInterval(context.Background(), info, time.Millisecond)

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("renew did not enter callback")
	}

	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel work")
	}

	premature := false
	select {
	case <-stopped:
		premature = true
	case <-time.After(40 * time.Millisecond):
	}

	close(release)

	released = true

	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("stop never joined")
	}
	// Even the old implementation gets time to finish before restoring globals.
	require.Eventually(
		t,
		func() bool { return pool.Stats().InUse == 0 },
		time.Second,
		time.Millisecond,
	)
	require.False(t, premature, "stop returned while its renewal cleanup was still running")
}

func TestAsyncUsageRenewalConcurrentStopAndIdentitySnapshot(t *testing.T) {
	for _, mode := range []string{"stops", "parent-cancel", "failure", "snapshot"} {
		t.Run(mode, func(t *testing.T) {
			db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "renew.db"))
			require.NoError(t, err)
			require.NoError(t, db.AutoMigrate(&model.AsyncUsageInfo{}))
			pool, err := db.DB()
			require.NoError(t, err)

			old := model.LogDB
			model.LogDB = db
			t.Cleanup(func() { model.LogDB = old; require.NoError(t, pool.Close()) })

			info := &model.AsyncUsageInfo{
				RequestID:       "original",
				Status:          model.AsyncUsageStatusPending,
				ProcessingToken: "original-token",
				NextPollAt:      time.Now().UTC().Add(time.Minute),
			}
			require.NoError(t, db.Create(info).Error)
			id := info.ID
			original := info.NextPollAt

			parent, cancel := context.WithCancel(t.Context())
			defer cancel()

			if mode == "snapshot" {
				work, stop := startAsyncUsageClaimRenewalAtInterval(
					parent,
					info,
					5*time.Millisecond,
				)
				info.ID = 999
				info.ProcessingToken = "replacement-token"
				info.RequestID = "changed"
				info.UpstreamID = "changed"

				require.Eventually(t, func() bool {
					var row model.AsyncUsageInfo

					e := db.First(&row, id).Error
					return e == nil && row.NextPollAt.After(original.Add(time.Minute))
				}, time.Second, time.Millisecond)
				require.NoError(t, work.Err())
				stop()
				require.True(
					t,
					info.NextPollAt.Equal(original),
					"renewal must not race business scheduling cursor",
				)

				var row model.AsyncUsageInfo
				require.NoError(t, db.First(&row, id).Error)
				require.Equal(t, "original-token", row.ProcessingToken)

				return
			}

			entered, release := make(chan struct{}), make(chan struct{})

			var once sync.Once
			defer func() { once.Do(func() { close(release) }) }()

			require.NoError(
				t,
				db.Callback().
					Update().
					After("gorm:update").
					Register("test:cleanup", func(tx *gorm.DB) {
						if tx.Statement.Table == "async_usage_infos" {
							close(entered)
							<-release

							if mode == "failure" {
								_ = tx.AddError(errors.New("synthetic renewal failure"))
							}
						}
					}),
			)
			defer func() {
				if err := db.Callback().Update().Remove("test:cleanup"); err != nil {
					t.Error(err)
				}
			}()

			work, stop := startAsyncUsageClaimRenewalAtInterval(parent, info, time.Millisecond)

			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("renewal not entered")
			}

			if mode == "parent-cancel" {
				cancel()
			}

			var (
				finished atomic.Int32
				wg       sync.WaitGroup
			)
			if mode != "failure" {
				for range 4 {
					wg.Go(func() { ; stop(); finished.Add(1) })
				}

				<-work.Done()
				time.Sleep(30 * time.Millisecond)
				require.Zero(t, finished.Load())
			}

			once.Do(func() { close(release) })

			if mode == "failure" {
				select {
				case <-work.Done():
				case <-time.After(time.Second):
					t.Fatal("failure did not cancel work")
				}

				stop()
			} else {
				wg.Wait()
				require.EqualValues(t, 4, finished.Load())
			}

			stop()

			var row model.AsyncUsageInfo
			require.NoError(t, db.First(&row, id).Error)
			require.True(
				t,
				row.NextPollAt.Equal(original),
				"canceled/failed renew rolls back before stop returns",
			)
		})
	}
}
