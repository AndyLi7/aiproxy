package model_test

import (
	"context"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestPostgresRecoveryHandoffRechecksAfterLocks(t *testing.T) {
	for _, phase := range []string{"accept", "release"} {
		for _, lock := range []string{"task", "handoff"} {
			outcomes := []string{"live", "expired", "version"}
			if lock == "task" {
				outcomes = []string{"live", "expired", "owner", "extended"}
			}
			for _, outcome := range outcomes {
				t.Run(phase+"/"+lock+"/"+outcome, func(t *testing.T) {
					db := queueLockPostgres(t)
					f := runtimeAdmissionOnFixture(t, admissionFixtureOn(t, db, ""))
					installRecoveryHandoff(t, db)
					deadline := time.Now().Add(3 * time.Second).Truncate(time.Second)
					task := recoveryHandoffTask(t, f, deadline)
					h, err := model.PrepareNativeRecoveryHandoff(t.Context(), db, task)
					require.NoError(t, err)
					if phase == "release" {
						accepted, e := h.Accept(t.Context())
						require.NoError(t, e)
						require.True(t, accepted)
					}
					row := recoveryHandoffRows(t, db)[0]
					pool, err := db.DB()
					require.NoError(t, err)
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					holder, err := pool.BeginTx(ctx, nil)
					require.NoError(t, err)
					defer holder.Rollback()
					table, id := "native_tasks", task.ID
					if lock == "handoff" {
						table, id = "channel_admission_recovery_handoffs", row.ID
					}
					var got string
					require.NoError(t, holder.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE id=$1 FOR UPDATE", id).Scan(&got))
					done := make(chan error, 1)
					go func() {
						if phase == "accept" {
							_, e := h.Accept(ctx)
							done <- e
						} else {
							_, e := h.ReleaseAndRecordExit(ctx, task, time.Now().Add(time.Minute), false)
							done <- e
						}
					}()
					var application string
					require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&application).Error)
					require.Eventually(t, func() bool {
						var count int64
						e := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", application).Scan(&count).Error
						return e == nil && count > 0
					}, 2*time.Second, 5*time.Millisecond)
					switch outcome {
					case "expired":
						if delay := time.Until(deadline.Add(40 * time.Millisecond)); delay > 0 {
							time.Sleep(delay)
						}
					case "owner":
						_, err = holder.ExecContext(ctx, "UPDATE native_tasks SET recovery_owner='next-claim' WHERE id=$1", task.ID)
						require.NoError(t, err)
					case "extended":
						_, err = holder.ExecContext(ctx, "UPDATE native_tasks SET recovery_until=$1 WHERE id=$2", deadline.Add(11*time.Minute).Unix(), task.ID)
						require.NoError(t, err)
					case "version":
						_, err = holder.ExecContext(ctx, "UPDATE channel_admission_recovery_handoffs SET version=99 WHERE id=$1", row.ID)
						require.NoError(t, err)
					}
					require.NoError(t, holder.Commit())
					err = <-done
					fresh, e := model.GetNativeTask(db, task.ID, task.GroupID, task.TokenID)
					require.NoError(t, e)
					if outcome == "live" || outcome == "extended" {
						require.NoError(t, err)
						if phase == "release" {
							require.Empty(t, fresh.RecoveryOwner)
							require.Zero(t, fresh.RecoveryUntil)
							require.Equal(t, "exited", recoveryHandoffRows(t, db)[0].State)
						} else {
							require.Equal(t, "accepted", recoveryHandoffRows(t, db)[0].State)
						}
					} else {
						require.Error(t, err)
						require.NotEmpty(t, fresh.RecoveryOwner)
						require.NotZero(t, fresh.RecoveryUntil)
						require.Equal(t, task.NextRecoveryAt, fresh.NextRecoveryAt)
						want := "offered"
						if phase == "release" {
							want = "accepted"
						}
						require.Equal(t, want, recoveryHandoffRows(t, db)[0].State)
						require.Equal(t, "active", nativeExecutionRows(t, db)[0].State)
					}
					require.Equal(t, "open", runtimeUses(t, db)[0].State)
				})
			}
		}
	}
}
