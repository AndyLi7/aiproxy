package model_test

import (
	"context"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
	"time"
)

func TestPostgresNativeExecutionRechecksOwnerAfterTaskLock(t *testing.T) {
	for _, phase := range []string{"begin", "return"} {
		for _, outcome := range []string{"live", "changed", "expired", "extended"} {
			t.Run(phase+"/"+outcome, func(t *testing.T) {
				db := queueLockPostgres(t)
				f := runtimeAdmissionOnFixture(t, admissionFixtureOn(t, db, ""))
				installNativeExecution(t, db)
				task := executionNativeTask(t, f)
				deadline := time.Now().Add(2 * time.Second).Truncate(time.Second)
				require.NoError(t, db.Model(&model.NativeTask{}).Where("id = ?", task.ID).Updates(map[string]any{"recovery_owner": "owner", "recovery_until": deadline.Unix()}).Error)
				var handle *model.NativeTaskExecutionHandle
				var err error
				if phase == "return" {
					handle, err = beginNativeExecution(context.Background(), db, task, model.NativeExecutionPoll, "owner")
					require.NoError(t, err)
				}
				pool, err := db.DB()
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				holder, err := pool.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer holder.Rollback()
				var id string
				require.NoError(t, holder.QueryRowContext(ctx, "SELECT id FROM native_tasks WHERE id=$1 FOR UPDATE", task.ID).Scan(&id))
				done := make(chan error, 1)
				go func() {
					if phase == "begin" {
						_, e := beginNativeExecution(ctx, db, task, model.NativeExecutionPoll, "owner")
						done <- e
					} else {
						done <- handle.RecordReturn(ctx, false)
					}
				}()
				var application string
				require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&application).Error)
				require.Eventually(t, func() bool {
					var count int64
					e := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", application).Scan(&count).Error
					return e == nil && count > 0
				}, 2*time.Second, 5*time.Millisecond)
				if outcome == "changed" {
					_, err = holder.ExecContext(ctx, "UPDATE native_tasks SET recovery_owner=$1 WHERE id=$2", "new-owner", task.ID)
					require.NoError(t, err)
				}
				if outcome == "extended" {
					_, err = holder.ExecContext(ctx, "UPDATE native_tasks SET recovery_until=$1 WHERE id=$2 AND recovery_owner=$3 AND recovery_until>$4", deadline.Add(11*time.Minute).Unix(), task.ID, "owner", time.Now().Unix())
					require.NoError(t, err)
				}
				if outcome == "expired" {
					if delay := time.Until(deadline.Add(40 * time.Millisecond)); delay > 0 {
						time.Sleep(delay)
					}
				}
				require.NoError(t, holder.Commit())
				err = <-done
				rows := nativeExecutionRows(t, db)
				if outcome == "live" || outcome == "extended" {
					require.NoError(t, err)
					require.Len(t, rows, 1)
					if phase == "return" {
						require.Equal(t, "returned", rows[0].State)
					} else {
						require.Equal(t, "active", rows[0].State)
					}
				} else {
					require.Error(t, err)
					if phase == "begin" {
						require.Empty(t, rows)
					} else {
						require.Len(t, rows, 1)
						require.Equal(t, "active", rows[0].State)
						require.Zero(t, rows[0].ReturnedAtMs)
					}
				}
				require.Equal(t, "open", runtimeUses(t, db)[0].State)
			})
		}
	}
}
func TestPostgresNativeExecutionConcurrentPollsRemainIndependent(t *testing.T) {
	db := queueLockPostgres(t)
	f := runtimeAdmissionOnFixture(t, admissionFixtureOn(t, db, ""))
	installNativeExecution(t, db)
	task := executionNativeTask(t, f)
	handles := make(chan *model.NativeTaskExecutionHandle, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, e := beginNativeExecution(context.Background(), db, task, model.NativeExecutionPoll, "")
			handles <- h
			errs <- e
		}()
	}
	wg.Wait()
	close(handles)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	rows := nativeExecutionRows(t, db)
	require.Len(t, rows, 2)
	require.NotEqual(t, rows[0].ID, rows[1].ID)
	require.Equal(t, rows[0].UseID, rows[1].UseID)
	first := <-handles
	second := <-handles
	require.NoError(t, first.RecordReturn(context.Background(), false))
	active := 0
	for _, row := range nativeExecutionRows(t, db) {
		if row.State == "active" {
			active++
		}
	}
	require.Equal(t, 1, active)
	require.NoError(t, second.RecordReturn(context.Background(), false))
	for _, row := range nativeExecutionRows(t, db) {
		require.Equal(t, "returned", row.State)
		require.Equal(t, "unproven", row.ContinuationState)
	}
	require.Equal(t, "open", runtimeUses(t, db)[0].State)
}

func TestPostgresNativeRecoveryExtensionCannotReviveExpiredLease(t *testing.T) {
	for _, outcome := range []string{"live", "expired", "reclaimed"} {
		t.Run(outcome, func(t *testing.T) {
			db := queueLockPostgres(t)
			require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
			_, _, err := model.ReserveNativeTask(db, model.NativeTask{ID: "extend", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "f", OutputSchema: `{}`})
			require.NoError(t, err)
			now := time.Now()
			claims, err := model.ClaimNativeRecovery(db, "worker", now, 1)
			require.NoError(t, err)
			require.Len(t, claims, 1)
			claim := claims[0]
			deadline := time.Now().Add(2 * time.Second).Truncate(time.Second)
			require.NoError(t, db.Model(&model.NativeTask{}).Where("id = ?", claim.ID).Update("recovery_until", deadline.Unix()).Error)
			pool, err := db.DB()
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			holder, err := pool.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer holder.Rollback()
			var id string
			require.NoError(t, holder.QueryRowContext(ctx, "SELECT id FROM native_tasks WHERE id=$1 FOR UPDATE", claim.ID).Scan(&id))
			target := now.Add(11 * time.Minute)
			done := make(chan error, 1)
			go func() { done <- model.ExtendNativeRecovery(db.WithContext(ctx), claim.ID, claim.RecoveryOwner, target) }()
			var application string
			require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&application).Error)
			require.Eventually(t, func() bool {
				var count int64
				e := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", application).Scan(&count).Error
				return e == nil && count > 0
			}, 2*time.Second, 5*time.Millisecond)
			if outcome == "expired" {
				if delay := time.Until(deadline.Add(40 * time.Millisecond)); delay > 0 {
					time.Sleep(delay)
				}
			}
			if outcome == "reclaimed" {
				_, err = holder.ExecContext(ctx, "UPDATE native_tasks SET recovery_owner=$1 WHERE id=$2", "new-claim", claim.ID)
				require.NoError(t, err)
			}
			require.NoError(t, holder.Commit())
			err = <-done
			current, e := model.GetNativeTask(db, claim.ID, "g", 1)
			require.NoError(t, e)
			if outcome == "live" {
				require.NoError(t, err)
				require.Equal(t, target.Unix(), current.RecoveryUntil)
			} else {
				require.Error(t, err)
				require.Equal(t, deadline.Unix(), current.RecoveryUntil)
			}
		})
	}
}
