package model_test

import (
	"context"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestPostgresImagePollHandoffLocks(t *testing.T) {
	for _, phase := range []string{"accept-journal", "renew-journal", "release-journal", "release-log", "return-journal", "return-protocol"} {
		for _, outcome := range []string{"live", "expired", "corrupt"} {
			t.Run(phase+"/"+outcome, func(t *testing.T) {
				db := queueLockPostgres(t)
				f := runtimeAdmissionOnFixture(t, admissionFixtureOn(t, db, ""))
				task, u := pollFixtureOn(t, f, 1400*time.Millisecond)
				require.NoError(t, db.Model(&model.Log{}).Where("id=?", u.LogID).Update("billing_operation_id", task.BillingOperationID).Error)
				if phase != "accept-journal" {
					ok, e := model.AcceptImagePoll(t.Context(), u)
					require.NoError(t, e)
					require.True(t, ok)
				}
				if phase == "return-journal" || phase == "return-protocol" {
					u.NextPollAt = time.Now().Add(time.Hour)
					require.NoError(t, model.TouchClaimedAsyncUsageInfo(u))
				}
				before := pollRows(t, db)[0]
				deadline := time.UnixMicro(before.ClaimUntilMicros)
				pool, e := db.DB()
				require.NoError(t, e)
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				holder, e := pool.BeginTx(ctx, nil)
				require.NoError(t, e)
				defer holder.Rollback()
				table := "channel_admission_image_poll_handoffs"
				var key any = before.ID
				if phase == "release-log" {
					table, key = "logs", u.LogID
				}
				if phase == "return-protocol" {
					table, key = "channel_admission_execution_protocols", 7
				}
				var id any
				require.NoError(t, holder.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE id=$1 FOR UPDATE", key).Scan(&id))
				done := make(chan error, 1)
				go func() {
					var err error
					switch phase {
					case "accept-journal":
						_, err = model.AcceptImagePoll(ctx, u)
					case "renew-journal":
						var ok bool
						ok, err = model.RenewImagePollClaim(ctx, u, time.Now().Add(3*time.Minute))
						if err == nil && !ok {
							err = model.ErrImagePollClaimLost
						}
					case "release-journal":
						u.NextPollAt = time.Now().Add(time.Hour)
						err = model.TouchClaimedAsyncUsageInfo(u)
					case "release-log":
						err = model.SetClaimedImageTaskResult(ctx, u, task, "failed", nil, &model.ImageTaskError{Code: "upstream_error", Message: "synthetic"})
					default:
						err = model.RecordImagePollReturn(ctx, u)
					}
					done <- err
				}()
				var app string
				require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&app).Error)
				require.Eventually(t, func() bool {
					var n int64
					e := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).Scan(&n).Error
					return e == nil && n > 0
				}, time.Second, 5*time.Millisecond)
				if outcome == "expired" {
					if delay := time.Until(deadline.Add(20 * time.Millisecond)); delay > 0 {
						time.Sleep(delay)
					}
				}
				if outcome == "corrupt" {
					switch table {
					case "logs":
						_, e = holder.ExecContext(ctx, "UPDATE logs SET billing_operation_id='different' WHERE id=$1", key)
					case "channel_admission_execution_protocols":
						_, e = holder.ExecContext(ctx, "UPDATE channel_admission_execution_protocols SET version=99 WHERE id=$1", key)
					default:
						_, e = holder.ExecContext(ctx, "UPDATE channel_admission_image_poll_handoffs SET authorization_hash='corrupt' WHERE id=$1", key)
					}
					require.NoError(t, e)
				}
				require.NoError(t, holder.Commit())
				err := <-done
				after := pollRows(t, db)[0]
				if outcome == "live" {
					require.NoError(t, err)
					if phase == "return-journal" || phase == "return-protocol" {
						require.Equal(t, "exited", after.State)
					}
				} else {
					require.Error(t, err)
					require.Equal(t, before.State, after.State)
					require.Equal(t, before.ReleaseJSON, after.ReleaseJSON)
					if phase == "release-log" {
						fresh, e := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
						require.NoError(t, e)
						require.Equal(t, task.Status, fresh.Status)
					}
				}
			})
		}
	}
}

// Late-loaded/legacy workers have no private capability and must not mint one,
// but their optional-protocol inspection still obeys cancellation while waiting.
func TestPostgresImagePollNoHandleAcceptCancellation(t *testing.T) {
	db := queueLockPostgres(t)
	f := runtimeAdmissionOnFixture(t, admissionFixtureOn(t, db, ""))
	_, claimed := pollFixtureOn(t, f, 3*time.Minute)
	var u model.AsyncUsageInfo
	require.NoError(t, db.First(&u, "id=?", claimed.ID).Error)
	before := pollRows(t, db)
	pool, e := db.DB()
	require.NoError(t, e)
	holder, e := pool.BeginTx(context.Background(), nil)
	require.NoError(t, e)
	defer holder.Rollback()
	var id int
	require.NoError(t, holder.QueryRow("SELECT id FROM channel_admission_execution_protocols WHERE id=7 FOR UPDATE").Scan(&id))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		ok, err := model.AcceptImagePoll(ctx, &u)
		if ok && err == nil {
			err = model.ErrChannelAdmissionConflict
		}
		done <- err
	}()
	var app string
	require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&app).Error)
	require.Eventually(t, func() bool {
		var n int64
		e := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).Scan(&n).Error
		return e == nil && n > 0
	}, time.Second, 5*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("no-handle Accept ignored cancellation behind protocol lock")
	}
	require.NoError(t, holder.Rollback())
	require.Equal(t, before, pollRows(t, db))
	ok, err := model.AcceptImagePoll(ctx, &u)
	require.False(t, ok)
	require.ErrorIs(t, err, context.Canceled)
}

func TestPostgresImagePollReceiptParentCancellation(t *testing.T) {
	db := queueLockPostgres(t)
	f := runtimeAdmissionOnFixture(t, admissionFixtureOn(t, db, ""))
	_, u := pollFixtureOn(t, f, 3*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ok, err := model.AcceptImagePoll(ctx, u)
	require.NoError(t, err)
	require.True(t, ok)
	// A duplicate Accept must not overwrite the original invocation's parent.
	ok, err = model.AcceptImagePoll(context.Background(), u)
	require.NoError(t, err)
	require.False(t, ok)
	before := pollRows(t, db)
	var usageBefore model.AsyncUsageInfo
	require.NoError(t, db.First(&usageBefore, u.ID).Error)
	var logBefore model.Log
	require.NoError(t, db.First(&logBefore, u.LogID).Error)
	pool, err := db.DB()
	require.NoError(t, err)
	holder, err := pool.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer holder.Rollback()
	var id int
	require.NoError(t, holder.QueryRow("SELECT id FROM channel_admission_execution_protocols WHERE id=7 FOR UPDATE").Scan(&id))
	done := make(chan error, 1)
	go func() { u.NextPollAt = time.Now().Add(time.Hour); done <- model.TouchClaimedAsyncUsageInfo(u) }()
	var app string
	require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&app).Error)
	require.Eventually(t, func() bool {
		var n int64
		e := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).Scan(&n).Error
		return e == nil && n > 0
	}, time.Second, 5*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("claimed receipt ignored parent cancellation behind protocol lock")
	}
	require.NoError(t, holder.Rollback())
	require.Equal(t, before, pollRows(t, db))
	var after model.AsyncUsageInfo
	require.NoError(t, db.First(&after, u.ID).Error)
	require.Equal(t, usageBefore, after)
	var logAfter model.Log
	require.NoError(t, db.First(&logAfter, u.LogID).Error)
	require.Equal(t, logBefore, logAfter)
}
