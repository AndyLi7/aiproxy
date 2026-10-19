package model_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func claimReceipt(t *testing.T, op string, info *model.AsyncUsageInfo) (bool, error) {
	t.Helper()

	switch op {
	case "fail":
		return model.FailClaimedAsyncUsageInfo(info)
	case "complete":
		return model.CompleteClaimedAsyncUsageInfo(
			info,
			model.Usage{},
			model.UsageContext{},
			model.Amount{},
		)
	case "touch":
		err := model.TouchClaimedAsyncUsageInfo(info)
		return err == nil, err
	case "retry":
		err := model.RetryClaimedAsyncUsageInfo(info)
		return err == nil, err
	case "attempt":
		err := model.MarkAsyncUsageBalanceConsumeAttempted(info)
		return err == nil, err
	case "consumed":
		err := model.MarkAsyncUsageBalanceConsumed(info)
		return err == nil, err
	default:
		err := model.PrepareClaimedAsyncUsageSettlement(
			info,
			model.Usage{},
			model.UsageContext{},
			model.Amount{},
		)

		return err == nil, err
	}
}

func TestAsyncUsageClaimReceiptFencing(t *testing.T) {
	for _, op := range []string{"fail", "complete", "touch", "retry", "attempt", "consumed", "prepare"} {
		t.Run(op, func(t *testing.T) {
			for _, mode := range []string{"live", "expired", "owner", "completed", "zero", "nil", "empty-token", "renewed", "expired-write"} {
				t.Run(mode, func(t *testing.T) {
					db := asyncRenewalDB(t)

					deadline := time.Now().UTC().Add(time.Minute).Truncate(time.Millisecond)
					if mode == "expired" {
						deadline = time.Now().UTC().Add(-time.Second)
					}

					if mode == "expired-write" {
						deadline = time.Now().UTC().Add(150 * time.Millisecond)
					}

					info := &model.AsyncUsageInfo{
						RequestID:       mode,
						Status:          model.AsyncUsageStatusPending,
						ProcessingToken: "original",
						NextPollAt:      deadline,
					}
					require.NoError(t, db.Create(info).Error)

					switch mode {
					case "owner":
						require.NoError(
							t,
							db.Model(&model.AsyncUsageInfo{}).
								Where("id=?", info.ID).
								Update("processing_token", "replacement").
								Error,
						)
					case "completed":
						require.NoError(
							t,
							db.Model(&model.AsyncUsageInfo{}).
								Where("id=?", info.ID).
								Update("status", model.AsyncUsageStatusCompleted).
								Error,
						)
					case "zero":
						require.NoError(
							t,
							db.Model(&model.AsyncUsageInfo{}).
								Where("id=?", info.ID).
								Update("next_poll_at", time.Time{}).
								Error,
						)
					case "renewed":
						ok, err := model.RenewAsyncUsageClaim(
							info.ID,
							info.ProcessingToken,
							deadline.Add(time.Minute),
						)
						require.NoError(t, err)
						require.True(t, ok)
					case "expired-write":
						require.NoError(
							t,
							db.Callback().
								Update().
								After("gorm:update").
								Register("test:receipt-expiry", func(tx *gorm.DB) { time.Sleep(time.Until(deadline.Add(25 * time.Millisecond))) }),
						)
						defer func() {
							if err := db.
								Callback().Update().
								Remove("test:receipt-expiry"); err != nil {
								t.Error(err)
							}
						}()
					}

					var before model.AsyncUsageInfo
					require.NoError(t, db.First(&before, info.ID).Error)
					id := info.ID
					// This is the next schedule, not the original authorization deadline.
					info.NextPollAt = time.Now().UTC().Add(10 * time.Minute)
					info.Error = "receipt"

					info.RetryCount = 3
					if mode == "nil" {
						info = nil
					}

					if mode == "empty-token" {
						info.ProcessingToken = ""
					}

					ok, err := claimReceipt(t, op, info)

					want := mode == "live" || mode == "renewed"
					if want || op == "fail" || op == "complete" {
						require.NoError(t, err)
					} else {
						require.Error(t, err)
					}

					require.Equal(t, want, ok)

					var after model.AsyncUsageInfo
					require.NoError(t, db.First(&after, id).Error)

					if !want {
						require.Equal(t, before, after)
					}
				})
			}
		})
	}
}

func TestPostgresAsyncUsageClaimReceiptLockAndFinalClock(t *testing.T) {
	for _, op := range []string{"fail", "complete", "touch"} {
		t.Run(op, func(t *testing.T) {
			for _, mode := range []string{"live", "expired-lock", "owner", "completed", "expired-write", "same-token-renewal"} {
				t.Run(mode, func(t *testing.T) {
					db := queueLockPostgres(t)
					require.NoError(t, db.AutoMigrate(&model.AsyncUsageInfo{}))

					old := model.LogDB
					model.LogDB = db
					t.Cleanup(func() { model.LogDB = old })

					deadline := time.Now().
						UTC().
						Add(1200 * time.Millisecond).
						Truncate(time.Microsecond)
					info := &model.AsyncUsageInfo{
						RequestID:       mode,
						Status:          model.AsyncUsageStatusPending,
						ProcessingToken: "original",
						NextPollAt:      deadline,
					}
					require.NoError(t, db.Create(info).Error)

					if mode == "expired-write" {
						require.NoError(
							t,
							db.Callback().
								Update().
								After("gorm:update").
								Register("test:receipt-expiry", func(tx *gorm.DB) { time.Sleep(time.Until(deadline.Add(25 * time.Millisecond))) }),
						)
						defer func() {
							if err := db.
								Callback().Update().
								Remove("test:receipt-expiry"); err != nil {
								t.Error(err)
							}
						}()
					}

					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()

					pool, err := db.DB()
					require.NoError(t, err)
					holder, err := pool.BeginTx(ctx, nil)
					require.NoError(t, err)

					defer func() {
						if err := holder.
							Rollback(); err !=
							nil &&
							!errors.
								Is(err, sql.ErrTxDone) {
							t.Error(err)
						}
					}()

					var id int
					require.NoError(
						t,
						holder.QueryRowContext(ctx, "SELECT id FROM async_usage_infos WHERE id=$1 FOR UPDATE", info.ID).
							Scan(&id),
					)
					info.NextPollAt = deadline.Add(10 * time.Minute)

					type result struct {
						ok  bool
						err error
					}

					done := make(chan result, 1)
					go func() { ok, e := claimReceipt(t, op, info); done <- result{ok, e} }()

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
					}, time.Second, 5*time.Millisecond)

					switch mode {
					case "expired-lock":
						time.Sleep(time.Until(deadline.Add(25 * time.Millisecond)))
					case "owner":
						_, err = holder.ExecContext(
							ctx,
							"UPDATE async_usage_infos SET processing_token='replacement' WHERE id=$1",
							info.ID,
						)
						require.NoError(t, err)
					case "completed":
						_, err = holder.ExecContext(
							ctx,
							"UPDATE async_usage_infos SET status=$1 WHERE id=$2",
							model.AsyncUsageStatusCompleted,
							info.ID,
						)
						require.NoError(t, err)
					case "same-token-renewal":
						_, err = holder.ExecContext(
							ctx,
							"UPDATE async_usage_infos SET next_poll_at=$1 WHERE id=$2",
							deadline.Add(time.Minute),
							info.ID,
						)
						require.NoError(t, err)
						time.Sleep(time.Until(deadline.Add(25 * time.Millisecond)))
					}

					require.NoError(t, holder.Commit())

					r := <-done

					want := mode == "live" || mode == "same-token-renewal"
					if want || op != "touch" {
						require.NoError(t, r.err)
					} else {
						require.Error(t, r.err)
					}

					require.Equal(t, want, r.ok)

					var after model.AsyncUsageInfo
					require.NoError(t, db.First(&after, id).Error)

					if !want {
						require.True(t, after.NextPollAt.Equal(deadline))

						if mode == "completed" {
							require.Equal(t, model.AsyncUsageStatusCompleted, after.Status)
						} else {
							require.Equal(t, model.AsyncUsageStatusPending, after.Status)
						}

						if mode == "owner" {
							require.Equal(t, "replacement", after.ProcessingToken)
						} else {
							require.Equal(t, "original", after.ProcessingToken)
						}
					}
				})
			}
		})
	}
}

func claimDeadlineFixture(t *testing.T, db *gorm.DB, deadline time.Time) *model.AsyncUsageInfo {
	t.Helper()
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))

	at := time.Now().UTC().Add(-time.Hour)
	require.NoError(
		t,
		db.Create(
			&model.ImageTask{
				ID:         "fenced-timeout",
				GroupID:    "group",
				TokenID:    1,
				Status:     "queued",
				UpstreamID: "upstream",
				CreatedAt:  at,
				QueuedAt:   &at,
			},
		).Error,
	)
	entry := &model.Log{
		RequestID:        "fenced-timeout",
		GroupID:          "group",
		TokenID:          1,
		AsyncUsageStatus: model.AsyncUsageStatusPending,
		Code:             202,
	}
	require.NoError(t, db.Create(entry).Error)
	info := &model.AsyncUsageInfo{
		RequestID:       "fenced-timeout",
		ImageTaskID:     "fenced-timeout",
		GroupID:         "group",
		TokenID:         1,
		LogID:           entry.ID,
		Status:          model.AsyncUsageStatusPending,
		ProcessingToken: "original",
		NextPollAt:      deadline,
	}
	require.NoError(t, db.Create(info).Error)

	return info
}

func assertTimeoutFenced(
	t *testing.T,
	db *gorm.DB,
	info *model.AsyncUsageInfo,
	success bool,
	owner string,
) {
	t.Helper()

	var (
		task  model.ImageTask
		usage model.AsyncUsageInfo
		entry model.Log
	)

	require.NoError(t, db.First(&task, "id=?", info.ImageTaskID).Error)
	require.NoError(t, db.First(&usage, info.ID).Error)
	require.NoError(t, db.First(&entry, info.LogID).Error)

	if success {
		require.Equal(t, "failed", task.Status)
		require.Equal(t, model.AsyncUsageStatusFailed, usage.Status)
		require.Empty(t, usage.ProcessingToken)
		require.Equal(t, model.AsyncUsageStatusFailed, entry.AsyncUsageStatus)
	} else {
		require.Equal(t, "queued", task.Status)
		require.Equal(t, model.AsyncUsageStatusPending, usage.Status)
		require.Equal(t, owner, usage.ProcessingToken)
		require.Equal(t, model.AsyncUsageStatusPending, entry.AsyncUsageStatus)
		require.Equal(t, 202, entry.Code)
	}
}

func TestImageGenerationTimeoutClaimFencing(t *testing.T) {
	for _, mode := range []string{"live", "expired", "owner", "expired-log-write", "wrong-task", "missing-log", "foreign-log"} {
		t.Run(mode, func(t *testing.T) {
			db := asyncRenewalDB(t)

			deadline := time.Now().UTC().Add(time.Minute)
			if mode == "expired" {
				deadline = time.Now().UTC().Add(-time.Second)
			}

			if mode == "expired-log-write" {
				deadline = time.Now().UTC().Add(200 * time.Millisecond)
			}

			info := claimDeadlineFixture(t, db, deadline)

			owner := "original"
			if mode == "owner" {
				owner = "replacement"
				require.NoError(
					t,
					db.Model(&model.AsyncUsageInfo{}).
						Where("id=?", info.ID).
						Update("processing_token", owner).
						Error,
				)
			}

			if mode == "expired-log-write" {
				require.NoError(
					t,
					db.Callback().
						Update().
						After("gorm:update").
						Register("test:timeout-expiry", func(tx *gorm.DB) {
							if tx.Statement.Table == "logs" {
								time.Sleep(time.Until(deadline.Add(25 * time.Millisecond)))
							}
						}),
				)
				defer func() {
					if err := db.
						Callback().Update().
						Remove("test:timeout-expiry"); err != nil {
						t.Error(err)
					}
				}()
			}

			if mode == "missing-log" {
				require.NoError(t, db.Delete(&model.Log{}, info.LogID).Error)
			}

			if mode == "foreign-log" {
				require.NoError(
					t,
					db.Model(&model.Log{}).
						Where("id=?", info.LogID).
						Update("group_id", "foreign").
						Error,
				)
			}

			id := info.ImageTaskID
			if mode == "wrong-task" {
				id = "different"
			}

			ok, err := model.ExpireImageGeneration(id, time.Now(), info)
			if mode == "missing-log" || mode == "foreign-log" {
				require.Error(t, err)
				require.False(t, ok)

				var (
					task  model.ImageTask
					usage model.AsyncUsageInfo
				)

				require.NoError(t, db.First(&task, "id=?", info.ImageTaskID).Error)
				require.NoError(t, db.First(&usage, info.ID).Error)
				require.Equal(t, "queued", task.Status)
				require.Equal(t, model.AsyncUsageStatusPending, usage.Status)
				require.Equal(t, "original", usage.ProcessingToken)

				if mode == "foreign-log" {
					var entry model.Log
					require.NoError(t, db.First(&entry, info.LogID).Error)
					require.Equal(t, "foreign", entry.GroupID)
					require.Equal(t, model.AsyncUsageStatusPending, entry.AsyncUsageStatus)
				}

				return
			}

			require.NoError(t, err)
			require.Equal(t, mode == "live", ok)
			assertTimeoutFenced(t, db, info, ok, owner)
		})
	}
}

func TestPostgresImageGenerationTimeoutClaimFencing(t *testing.T) {
	for _, mode := range []string{"live-log", "expired-log", "expired-task", "owner"} {
		t.Run(mode, func(t *testing.T) {
			db := queueLockPostgres(t)
			old := model.LogDB
			model.LogDB = db
			t.Cleanup(func() { model.LogDB = old })

			deadline := time.Now().UTC().Add(1200 * time.Millisecond)
			info := claimDeadlineFixture(t, db, deadline)

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			pool, err := db.DB()
			require.NoError(t, err)
			holder, err := pool.BeginTx(ctx, nil)
			require.NoError(t, err)

			defer func() {
				if err := holder.
					Rollback(); err !=
					nil &&
					!errors.
						Is(err, sql.ErrTxDone) {
					t.Error(err)
				}
			}()

			if mode == "owner" || mode == "expired-task" {
				var id string
				require.NoError(
					t,
					holder.QueryRowContext(ctx, "SELECT id FROM image_tasks WHERE id=$1 FOR UPDATE", info.ImageTaskID).
						Scan(&id),
				)
			} else {
				var id int
				require.NoError(
					t,
					holder.QueryRowContext(ctx, "SELECT id FROM logs WHERE id=$1 FOR UPDATE", info.LogID).
						Scan(&id),
				)
			}

			type result struct {
				ok  bool
				err error
			}

			done := make(chan result, 1)
			go func() {
				ok, e := model.ExpireImageGeneration(info.ImageTaskID, time.Now(), info)
				done <- result{ok, e}
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
			}, time.Second, 5*time.Millisecond)

			owner := "original"
			if mode == "owner" {
				owner = "replacement"
				_, err = holder.ExecContext(
					ctx,
					"UPDATE async_usage_infos SET processing_token=$1 WHERE id=$2",
					owner,
					info.ID,
				)
				require.NoError(t, err)
			} else if mode != "live-log" {
				time.Sleep(time.Until(deadline.Add(25 * time.Millisecond)))
			}

			require.NoError(t, holder.Commit())

			r := <-done
			require.NoError(t, r.err)
			require.Equal(t, mode == "live-log", r.ok)
			assertTimeoutFenced(t, db, info, r.ok, owner)
		})
	}
}
