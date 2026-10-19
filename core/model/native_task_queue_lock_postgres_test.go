package model_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Opt-in only: the harness must supply a fresh local, disposable PostgreSQL.
// This test never starts a container, contacts a remote host or migrates an
// existing application schema. Each case owns and drops its own empty schema.
func queueLockPostgres(t *testing.T) *gorm.DB {
	t.Helper()

	dsn := os.Getenv("B2_G3_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires a disposable local PostgreSQL via B2_G3_TEST_POSTGRES_DSN")
	}

	u, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"127.0.0.1", "localhost", "::1"}, u.Hostname())
	require.Equal(t, "/g3_queue_test", u.Path)

	admin, err := model.OpenPostgreSQL(dsn)
	require.NoError(t, err)

	schema := fmt.Sprintf("g3_lock_%d", time.Now().UnixNano())
	require.NoError(t, admin.Exec("CREATE SCHEMA "+schema).Error)

	query := u.Query()
	query.Set("search_path", schema)
	query.Set("application_name", schema)
	u.RawQuery = query.Encode()
	db, err := model.OpenPostgreSQL(u.String())
	require.NoError(t, err)
	t.Cleanup(func() {
		pool, e := db.DB()
		require.NoError(t, e)
		require.NoError(t, pool.Close())
		require.NoError(t, admin.Exec("DROP SCHEMA "+schema+" CASCADE").Error)
		pool, e = admin.DB()
		require.NoError(t, e)
		require.NoError(t, pool.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}, &model.ImageTask{}))

	return db
}

func TestPostgresNativeExpiryAfterRowLockWait(t *testing.T) {
	for _, transition := range []string{"claim", "submit"} {
		for _, outcome := range []string{"expired", "live", "cancelled"} {
			t.Run(transition+"/"+outcome, func(t *testing.T) {
				db := queueLockPostgres(t)
				enqueuedNativeTask(t, db, "locked")

				timeout := time.Minute
				if outcome == "expired" {
					timeout = time.Second
				}

				deadline := time.Now().Add(timeout)
				if transition == "claim" {
					require.NoError(
						t,
						db.Model(&model.NativeTask{}).
							Where("id = ?", "locked").
							Update("enqueued_at", deadline.Add(-timeout).UTC()).
							Error,
					)
				} else {
					claimed, _, err := model.ClaimNativeDispatch(
						db,
						model.NativeDispatchClaim{
							Scope: "fal:default",
							Owner: "worker",
							Now:   time.Now(),
							Pick:  func(s *model.NativeQueueSnapshot) string { return s.Waiting[0].ID },
						},
					)
					require.NoError(t, err)
					require.NotNil(t, claimed)
					require.NoError(
						t,
						db.Model(&model.NativeTask{}).
							Where("id = ?", "locked").
							Update("dispatch_until", deadline.UTC()).
							Error,
					)
				}

				pool, err := db.DB()
				require.NoError(t, err)

				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				holder, err := pool.BeginTx(ctx, nil)
				require.NoError(t, err)

				defer func() {
					if err := holder.
						Rollback(); err !=
						nil &&
						!errors.
							Is(err,
								sql.ErrTxDone,
							) {
						t.Error(err)
					}
				}()

				var id string
				require.NoError(
					t,
					holder.QueryRowContext(ctx, "SELECT id FROM native_tasks WHERE id = $1 FOR UPDATE", "locked").
						Scan(&id),
				)

				type result struct {
					task *model.NativeTask
					err  error
				}

				done := make(chan result, 1)
				go func() {
					if transition == "claim" {
						task, _, callErr := model.ClaimNativeDispatch(
							db.WithContext(ctx),
							model.NativeDispatchClaim{
								Scope:       "fal:default",
								Owner:       "worker",
								Now:         time.Now(),
								WaitTimeout: timeout,
								Pick:        func(s *model.NativeQueueSnapshot) string { return s.Waiting[0].ID },
							},
						)
						done <- result{task, callErr}
					} else {
						done <- result{nil, model.StartNativeDispatchSubmission(db.WithContext(ctx), "locked", "g", 1, "worker", time.Now())}
					}
				}()

				var application string
				require.NoError(
					t,
					db.Raw("SELECT current_setting('application_name')").Scan(&application).Error,
				)
				require.Eventually(t, func() bool {
					var waiting int64

					err := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name = ? AND wait_event_type = 'Lock'", application).
						Scan(&waiting).
						Error

					return err == nil && waiting > 0
				}, 2*time.Second, 5*time.Millisecond, "real second connection must be waiting on the row lock")

				if outcome == "expired" {
					// Do not update the held row: PostgreSQL must not rely on EvalPlanQual
					// due to a row-version change to notice that time passed while blocked.
					if delay := time.Until(deadline.Add(100 * time.Millisecond)); delay > 0 {
						time.Sleep(delay)
					}
				}

				if outcome == "cancelled" {
					_, err = holder.ExecContext(
						ctx,
						"UPDATE native_tasks SET status='failed', billing_settled=true WHERE id=$1",
						"locked",
					)
					require.NoError(t, err)
				}

				require.NoError(t, holder.Commit())

				got := <-done
				stored, err := model.GetNativeTask(db, "locked", "g", 1)
				require.NoError(t, err)

				if outcome == "live" {
					require.NoError(t, got.err)

					if transition == "claim" {
						require.NotNil(t, got.task)
						require.Equal(t, model.NativeStatusDispatching, stored.Status)
					} else {
						require.Equal(t, "submitting", stored.Status)
					}
				} else {
					if transition == "claim" {
						require.NoError(t, got.err)
						require.Nil(t, got.task)
					} else {
						require.ErrorIs(t, got.err, model.ErrNativeTaskConflict)
					}

					switch {
					case outcome == "cancelled":
						require.Equal(t, "failed", stored.Status)
						require.True(t, stored.BillingSettled)
					case transition == "claim":
						require.Equal(t, model.NativeStatusWaiting, stored.Status)
					default:
						require.Equal(t, model.NativeStatusDispatching, stored.Status)
					}
				}
			})
		}
	}
}
