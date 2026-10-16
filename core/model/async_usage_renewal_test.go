package model_test

import (
	"context"
	"errors"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"path/filepath"
	"testing"
	"time"
)

func asyncRenewalDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "lease.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.AsyncUsageInfo{}))
	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old; pool, e := db.DB(); require.NoError(t, e); require.NoError(t, pool.Close()) })
	return db
}
func TestAsyncUsageRenewalCannotReviveExpiredClaim(t *testing.T) {
	db := asyncRenewalDB(t)
	old := time.Now().UTC().Add(-time.Second)
	info := &model.AsyncUsageInfo{RequestID: "expired", Status: model.AsyncUsageStatusPending, ProcessingToken: "synthetic-owner", NextPollAt: old}
	require.NoError(t, db.Create(info).Error)
	ok, err := model.RenewAsyncUsageClaim(info.ID, info.ProcessingToken, time.Now().UTC().Add(time.Minute))
	require.NoError(t, err)
	require.False(t, ok, "expired owner cannot renew without a new claim")
	var current model.AsyncUsageInfo
	require.NoError(t, db.First(&current, info.ID).Error)
	require.True(t, current.NextPollAt.Equal(old))
}
func TestAsyncUsageRenewalSameSecondReclaimRejectsOldCleanup(t *testing.T) {
	db := asyncRenewalDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	info := &model.AsyncUsageInfo{RequestID: "same-worker", Status: model.AsyncUsageStatusPending, NextPollAt: now.Add(-time.Second)}
	require.NoError(t, db.Create(info).Error)
	ok, err := model.TryClaimAsyncUsageInfo(info, "first-token", now.Add(time.Minute), now)
	require.NoError(t, err)
	require.True(t, ok)
	// Fault-inject expiry without changing worker/time, then use actual claim API.
	require.NoError(t, db.Model(info).Update("next_poll_at", now.Add(-time.Second)).Error)
	next := &model.AsyncUsageInfo{ID: info.ID}
	ok, err = model.TryClaimAsyncUsageInfo(next, "second-token", now.Add(time.Minute), now)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = model.RenewAsyncUsageClaim(info.ID, info.ProcessingToken, now.Add(2*time.Minute))
	require.NoError(t, err)
	require.False(t, ok)
	require.Error(t, model.TouchClaimedAsyncUsageInfo(info))
	require.Error(t, model.RetryClaimedAsyncUsageInfo(info))
	ok, err = model.FailClaimedAsyncUsageInfo(info)
	require.NoError(t, err)
	require.False(t, ok)
	var current model.AsyncUsageInfo
	require.NoError(t, db.First(&current, info.ID).Error)
	require.Equal(t, next.ProcessingToken, current.ProcessingToken)
	require.True(t, current.NextPollAt.Equal(next.NextPollAt))
}

func TestAsyncUsageRenewalTransitions(t *testing.T) {
	for _, mode := range []string{"live-twice", "null-deadline", "zero-deadline", "completed", "missing", "shorter", "equal", "cancelled", "write-failure", "expiry-after-write"} {
		t.Run(mode, func(t *testing.T) {
			db := asyncRenewalDB(t)
			deadline := time.Now().UTC().Add(time.Minute)
			if mode == "expiry-after-write" {
				deadline = time.Now().UTC().Add(200 * time.Millisecond)
			}
			info := &model.AsyncUsageInfo{RequestID: mode, Status: model.AsyncUsageStatusPending, ProcessingToken: "owner", NextPollAt: deadline}
			require.NoError(t, db.Create(info).Error)
			proposal := deadline.Add(time.Minute)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "null-deadline":
				require.NoError(t, db.Model(info).Update("next_poll_at", nil).Error)
			case "zero-deadline":
				require.NoError(t, db.Model(info).Update("next_poll_at", time.Time{}).Error)
			case "completed":
				require.NoError(t, db.Model(info).Update("status", model.AsyncUsageStatusCompleted).Error)
			case "missing":
				require.NoError(t, db.Delete(info).Error)
			case "shorter":
				proposal = deadline.Add(-time.Second)
			case "equal":
				proposal = deadline
			case "cancelled":
				cancel()
			case "write-failure", "expiry-after-write":
				require.NoError(t, db.Callback().Update().After("gorm:update").Register("test:renew", func(tx *gorm.DB) {
					if tx.Statement.Table == "async_usage_infos" {
						if mode == "write-failure" {
							tx.AddError(errors.New("synthetic write failure"))
						} else {
							time.Sleep(time.Until(deadline.Add(25 * time.Millisecond)))
						}
					}
				}))
				defer db.Callback().Update().Remove("test:renew")
			}
			var before model.AsyncUsageInfo
			if mode != "missing" {
				require.NoError(t, db.First(&before, info.ID).Error)
			}
			ok, err := model.RenewAsyncUsageClaimContext(ctx, info.ID, info.ProcessingToken, proposal)
			if mode == "cancelled" || mode == "write-failure" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if mode == "live-twice" {
				require.True(t, ok)
				next := proposal.Add(time.Minute)
				ok, err = model.RenewAsyncUsageClaimContext(ctx, info.ID, info.ProcessingToken, next)
				require.NoError(t, err)
				require.True(t, ok)
				var row model.AsyncUsageInfo
				require.NoError(t, db.First(&row, info.ID).Error)
				require.True(t, row.NextPollAt.Equal(next.Truncate(time.Microsecond)))
				return
			}
			require.False(t, ok)
			if mode != "missing" {
				var after model.AsyncUsageInfo
				require.NoError(t, db.First(&after, info.ID).Error)
				require.Equal(t, before, after)
			}
		})
	}
}
func TestPostgresAsyncUsageRenewalLockAndFinalClock(t *testing.T) {
	for _, mode := range []string{"live", "expired-lock", "owner", "completed", "cancelled-lock", "expired-write"} {
		t.Run(mode, func(t *testing.T) {
			db := queueLockPostgres(t)
			require.NoError(t, db.AutoMigrate(&model.AsyncUsageInfo{}))
			oldDB := model.LogDB
			model.LogDB = db
			t.Cleanup(func() { model.LogDB = oldDB })
			deadline := time.Now().UTC().Add(1200 * time.Millisecond).Truncate(time.Microsecond)
			info := &model.AsyncUsageInfo{RequestID: mode, Status: model.AsyncUsageStatusPending, ProcessingToken: "owner", NextPollAt: deadline}
			require.NoError(t, db.Create(info).Error)
			if mode == "expired-write" {
				require.NoError(t, db.Callback().Update().After("gorm:update").Register("test:final", func(tx *gorm.DB) {
					if tx.Statement.Table == "async_usage_infos" {
						time.Sleep(time.Until(deadline.Add(25 * time.Millisecond)))
					}
				}))
				defer db.Callback().Update().Remove("test:final")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			pool, err := db.DB()
			require.NoError(t, err)
			holder, err := pool.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer holder.Rollback()
			var id int
			require.NoError(t, holder.QueryRowContext(ctx, "SELECT id FROM async_usage_infos WHERE id=$1 FOR UPDATE", info.ID).Scan(&id))
			workCtx, stop := context.WithCancel(ctx)
			defer stop()
			type result struct {
				ok  bool
				err error
			}
			done := make(chan result, 1)
			go func() {
				ok, e := model.RenewAsyncUsageClaimContext(workCtx, info.ID, info.ProcessingToken, deadline.Add(time.Minute))
				done <- result{ok, e}
			}()
			var app string
			require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&app).Error)
			require.Eventually(t, func() bool {
				var n int64
				e := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).Scan(&n).Error
				return e == nil && n > 0
			}, time.Second, 5*time.Millisecond)
			switch mode {
			case "expired-lock":
				time.Sleep(time.Until(deadline.Add(25 * time.Millisecond)))
			case "owner":
				_, err = holder.ExecContext(ctx, "UPDATE async_usage_infos SET processing_token='new-owner' WHERE id=$1", info.ID)
				require.NoError(t, err)
			case "completed":
				_, err = holder.ExecContext(ctx, "UPDATE async_usage_infos SET status=$1 WHERE id=$2", model.AsyncUsageStatusCompleted, info.ID)
				require.NoError(t, err)
			case "cancelled-lock":
				stop()
				select {
				case r := <-done:
					require.Error(t, r.err)
					require.False(t, r.ok)
				case <-time.After(time.Second):
					t.Fatal("cancelled SQL lock wait did not exit")
				}
			}
			require.NoError(t, holder.Commit())
			if mode != "cancelled-lock" {
				r := <-done
				require.NoError(t, r.err)
				require.Equal(t, mode == "live", r.ok)
			}
			var current model.AsyncUsageInfo
			require.NoError(t, db.First(&current, info.ID).Error)
			if mode == "live" {
				require.True(t, current.NextPollAt.Equal(deadline.Add(time.Minute)))
			} else {
				require.True(t, current.NextPollAt.Equal(deadline), "never commit renewal based on its own new deadline")
			}
		})
	}
}
