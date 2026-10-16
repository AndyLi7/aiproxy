package model_test

import (
	"context"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func TestPostgresImageRecoveryClaimAfterTaskLock(t *testing.T) {
	for _, phase := range []string{"claim", "validate", "billing", "release"} {
		outcomes := []string{"live", "expired", "owner", "generation"}
		if phase == "claim" {
			outcomes = []string{"live", "generation", "settled"}
		}
		for _, outcome := range outcomes {
			t.Run(phase+"/"+outcome, func(t *testing.T) {
				db := queueLockPostgres(t)
				f := runtimeAdmissionOnFixture(t, admissionFixtureOn(t, db, ""))
				installImageRecoveryClaim(t, db)
				task := imageRecoveryTask(t, f)
				deadline := time.Now().Add(2 * time.Second)
				if phase != "claim" {
					won, err := model.ClaimImagePrepaymentRecovery(t.Context(), task, time.Now())
					require.NoError(t, err)
					require.True(t, won)
					require.NoError(t, model.ShortenImageRecoveryDeadlineForTest(task, deadline))
				}
				pool, err := db.DB()
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				holder, err := pool.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer holder.Rollback()
				var id string
				require.NoError(t, holder.QueryRowContext(ctx, "SELECT id FROM image_tasks WHERE id=$1 FOR UPDATE", task.ID).Scan(&id))
				done := make(chan error, 1)
				go func() {
					var err error
					switch phase {
					case "claim":
						_, err = model.ClaimImagePrepaymentRecovery(ctx, task, time.Now().Add(-time.Hour))
					case "validate":
						err = model.ValidateImagePrepaymentRecovery(ctx, task)
					case "billing":
						task.Billing = &model.ImageTaskBilling{Status: "refunded"}
						err = model.SaveImageTaskBilling(task)
					case "release":
						err = model.ReleaseImagePrepaymentRecovery(ctx, task)
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
				switch outcome {
				case "expired":
					if delay := time.Until(deadline.Add(30 * time.Millisecond)); delay > 0 {
						time.Sleep(delay)
					}
				case "owner":
					_, err = holder.ExecContext(ctx, "UPDATE channel_admission_image_recovery_claims SET owner=$1", strings.Repeat("b", 64))
					require.NoError(t, err)
				case "generation":
					_, err = holder.ExecContext(ctx, "UPDATE image_tasks SET created_at=created_at+interval '1 second' WHERE id=$1", task.ID)
					require.NoError(t, err)
				case "settled":
					_, err = holder.ExecContext(ctx, "UPDATE image_tasks SET billing_settled=true WHERE id=$1", task.ID)
					require.NoError(t, err)
				}
				releasedAt := time.Now()
				require.NoError(t, holder.Commit())
				err = <-done
				rows := imageClaimRows(t, db)
				if outcome == "live" {
					require.NoError(t, err)
					require.Len(t, rows, 1)
					if phase == "claim" {
						require.Greater(t, rows[0].UntilMicros, releasedAt.Add(119*time.Second).UnixMicro())
					}
					if phase == "release" {
						require.Empty(t, rows[0].Owner)
					}
				} else if phase == "claim" && outcome == "settled" {
					require.NoError(t, err)
					require.Empty(t, rows)
				} else {
					require.Error(t, err)
					if phase == "claim" {
						require.Empty(t, rows)
					} else {
						require.NotEmpty(t, rows[0].Owner)
						fresh, e := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
						require.NoError(t, e)
						require.Nil(t, fresh.Billing)
					}
				}
				require.Equal(t, "open", runtimeUses(t, db)[0].State)
			})
		}
	}
}

func TestPostgresImageRecoveryClaimSidecarLock(t *testing.T) {
	for _, phase := range []string{"validate", "release", "reclaim"} {
		outcomes := []string{"live", "expired", "owner"}
		if phase == "reclaim" {
			outcomes = []string{"expired"}
		}
		for _, outcome := range outcomes {
			t.Run(phase+"/"+outcome, func(t *testing.T) {
				db := queueLockPostgres(t)
				f := runtimeAdmissionOnFixture(t, admissionFixtureOn(t, db, ""))
				installImageRecoveryClaim(t, db)
				task := imageRecoveryTask(t, f)
				won, err := model.ClaimImagePrepaymentRecovery(t.Context(), task, time.Now())
				require.NoError(t, err)
				require.True(t, won)
				deadline := time.Now().Add(2 * time.Second)
				require.NoError(t, model.ShortenImageRecoveryDeadlineForTest(task, deadline))
				before := imageClaimRows(t, db)[0]
				fresh, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
				require.NoError(t, err)
				pool, err := db.DB()
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				holder, err := pool.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer holder.Rollback()
				var id string
				require.NoError(t, holder.QueryRowContext(ctx, "SELECT id FROM channel_admission_image_recovery_claims WHERE id=$1 FOR UPDATE", before.ID).Scan(&id))
				done := make(chan error, 1)
				go func() {
					switch phase {
					case "validate":
						done <- model.ValidateImagePrepaymentRecovery(ctx, task)
					case "release":
						done <- model.ReleaseImagePrepaymentRecovery(ctx, task)
					case "reclaim":
						_, e := model.ClaimImagePrepaymentRecovery(ctx, fresh, time.Now().Add(-time.Hour))
						done <- e
					}
				}()
				var app string
				require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&app).Error)
				require.Eventually(t, func() bool {
					var n int64
					e := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).Scan(&n).Error
					return e == nil && n > 0
				}, time.Second, 5*time.Millisecond)
				if outcome == "expired" {
					if delay := time.Until(deadline.Add(40 * time.Millisecond)); delay > 0 {
						time.Sleep(delay)
					}
				}
				if outcome == "owner" {
					_, err = holder.ExecContext(ctx, "UPDATE channel_admission_image_recovery_claims SET owner=$1 WHERE id=$2", strings.Repeat("b", 64), before.ID)
					require.NoError(t, err)
				}
				releasedAt := time.Now()
				require.NoError(t, holder.Commit())
				err = <-done
				row := imageClaimRows(t, db)[0]
				if phase == "reclaim" {
					require.NoError(t, err)
					require.NotEqual(t, before.Owner, row.Owner)
					require.Greater(t, row.UntilMicros, releasedAt.Add(119*time.Second).UnixMicro())
					require.Error(t, model.ReleaseImagePrepaymentRecovery(ctx, task))
					require.NoError(t, model.ValidateImagePrepaymentRecovery(ctx, fresh))
					return
				}
				if outcome == "live" {
					require.NoError(t, err)
					if phase == "release" {
						require.Empty(t, row.Owner)
					}
				} else {
					require.Error(t, err)
					require.NotEmpty(t, row.Owner)
					require.Equal(t, before.UntilMicros, row.UntilMicros)
				}
			})
		}
	}
}
