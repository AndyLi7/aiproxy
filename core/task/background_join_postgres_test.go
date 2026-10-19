//nolint:testpackage // White-box coverage needs the package-private renewalTaskPostgres fixture or implementation.
package task

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// Real row-lock waits: cancellation reaches the claim SQL, returns while the
// lock is still held, and never installs a new owner/deadline after release.
func TestPostgresBackgroundCancelledClaimLockWait(t *testing.T) {
	for _, lane := range []string{"async", "native"} {
		t.Run(lane, func(t *testing.T) {
			db := renewalTaskPostgres(t)
			require.NoError(t, db.AutoMigrate(&model.NativeTask{}))

			old := model.LogDB
			model.LogDB = db
			t.Cleanup(func() { model.LogDB = old })

			now := time.Now().UTC().Truncate(time.Microsecond)
			info := model.AsyncUsageInfo{
				Status:     model.AsyncUsageStatusPending,
				NextPollAt: now.Add(-time.Minute),
			}
			native := model.NativeTask{
				ID:             "cancelled-claim",
				GroupID:        "g",
				TokenID:        1,
				Status:         "submitted",
				NextRecoveryAt: now.Add(-time.Minute).Unix(),
			}

			require.NoError(t, db.Create(&info).Error)
			require.NoError(t, db.Create(&native).Error)
			pool, err := db.DB()
			require.NoError(t, err)

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			holder, err := pool.BeginTx(ctx, nil)
			require.NoError(t, err)

			defer func() {
				if err := holder.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
					t.Error(err)
				}
			}()

			if lane == "async" {
				var id int
				require.NoError(
					t,
					holder.QueryRowContext(ctx, "SELECT id FROM async_usage_infos WHERE id=$1 FOR UPDATE", info.ID).
						Scan(&id),
				)
			} else {
				var id string
				require.NoError(
					t,
					holder.QueryRowContext(ctx, "SELECT id FROM native_tasks WHERE id=$1 FOR UPDATE", native.ID).
						Scan(&id),
				)
			}

			child, stop := context.WithCancel(ctx)
			defer stop()

			returned := make(chan error, 1)
			go func() {
				if lane == "async" {
					_, e := claimAsyncUsageContext(child, &info)
					returned <- e
				} else {
					_, e := model.ClaimNativeRecovery(db.WithContext(child), "worker", now, 1)
					returned <- e
				}
			}()

			var app string
			require.NoError(
				t,
				db.Raw("SELECT current_setting('application_name')").Scan(&app).Error,
			)
			require.Eventually(t, func() bool {
				var n int64

				e := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).
					Scan(&n).
					Error

				return e == nil && n > 0
			}, 2*time.Second, 5*time.Millisecond)
			stop()

			select {
			case err := <-returned:
				require.Error(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("claim ignored cancellation while waiting on row lock")
			}

			require.NoError(t, holder.Commit())

			var current model.AsyncUsageInfo
			require.NoError(t, db.First(&current, info.ID).Error)
			require.Empty(t, current.ProcessingToken)
			require.True(t, current.NextPollAt.Equal(now.Add(-time.Minute)))

			var currentNative model.NativeTask
			require.NoError(t, db.First(&currentNative, "id = ?", native.ID).Error)
			require.Empty(t, currentNative.RecoveryOwner)
			require.Zero(t, currentNative.RecoveryUntil)
		})
	}
}
