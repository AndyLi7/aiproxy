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

func TestPostgresAdmissionRuntimeReleaseRechecksAfterTaskLock(t *testing.T) {
	for _, action := range []string{"release", "positive", "abandon"} {
		for _, outcome := range []string{"expired", "owner-changed", "live"} {
			t.Run(action+"/"+outcome, func(t *testing.T) {
				db := queueLockPostgres(t)
				f := runtimeAdmissionOnFixture(t, admissionFixtureOn(t, db, ""))
				input := runtimeNativeTask(f, "locked", 0)
				input.CreatedAt = time.Now().Add(-20 * time.Minute)
				first, _, err := model.ReserveNativeTask(db, input)
				require.NoError(t, err)

				deadline := time.Now().Add(time.Minute)
				if outcome == "expired" {
					deadline = time.Now().Add(1500 * time.Millisecond)
				}

				require.NoError(
					t,
					db.Model(&model.NativeTask{}).
						Where("id = ?", first.ID).
						Updates(map[string]any{"recovery_owner": "worker", "recovery_until": deadline.Unix()}).
						Error,
				)
				evidence, err := model.CaptureNativeReservationRelease(db, first, "worker")
				require.NoError(t, err)
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
							Is(err, sql.ErrTxDone) {
						t.Error(err)
					}
				}()

				var id string
				require.NoError(
					t,
					holder.QueryRowContext(ctx, "SELECT id FROM native_tasks WHERE id=$1 FOR UPDATE", first.ID).
						Scan(&id),
				)

				done := make(chan error, 1)
				go func() {
					if action != "release" {
						first.AdmissionRecoveryOwner = "worker"
						if action == "abandon" {
							abandoned, err := model.AbandonNativeReservationGeneration(
								db,
								first,
								time.Now(),
								first.CreatedAt.Add(5*time.Minute),
							)
							if err == nil && !abandoned {
								err = model.ErrNativeTaskConflict
							}

							done <- err

							return
						}

						done <- model.WithNativeReservationGeneration(db, first, func(tx *gorm.DB) error {
							return model.TransitionNativeSubmission(tx, first.ID, first.GroupID, first.TokenID, "reserved", "submitting", "")
						})

						return
					}

					released, err := model.ReleaseNativeReservation(
						db,
						first.ID,
						first.GroupID,
						first.TokenID,
						time.Now().Add(time.Hour),
						evidence,
					)
					if err == nil && !released {
						err = model.ErrChannelAdmissionConflict
					}

					done <- err
				}()

				var application string
				require.NoError(
					t,
					db.Raw("SELECT current_setting('application_name')").Scan(&application).Error,
				)
				require.Eventually(t, func() bool {
					var count int64

					err := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", application).
						Scan(&count).
						Error

					return err == nil && count > 0
				}, 2*time.Second, 5*time.Millisecond)

				if outcome == "expired" {
					if delay := time.Until(deadline.Add(100 * time.Millisecond)); delay > 0 {
						time.Sleep(delay)
					}
				}

				if outcome == "owner-changed" {
					_, err = holder.ExecContext(
						ctx,
						"UPDATE native_tasks SET recovery_owner=$1 WHERE id=$2",
						"replacement",
						first.ID,
					)
					require.NoError(t, err)
				}

				require.NoError(t, holder.Commit())

				err = <-done
				uses := runtimeUses(t, db)
				require.Len(t, uses, 1)

				var count int64
				require.NoError(t, db.Model(&model.NativeTask{}).Count(&count).Error)

				if outcome == "live" {
					require.NoError(t, err)

					if action == "release" {
						require.Zero(t, count)
						require.Equal(t, "closed", uses[0].State)
					} else {
						var current model.NativeTask
						require.NoError(t, db.First(&current, "id = ?", first.ID).Error)

						if action == "abandon" {
							require.Equal(t, "failed", current.Status)
							require.Equal(t, model.NotStartedCode, current.ErrorCode)
						} else {
							require.Equal(t, "submitting", current.Status)
						}

						require.Equal(t, "open", uses[0].State)
					}
				} else {
					if action == "release" {
						require.ErrorIs(t, err, model.ErrChannelAdmissionConflict)
					} else {
						require.ErrorIs(t, err, model.ErrNativeTaskConflict)
					}

					require.EqualValues(t, 1, count)
					require.Equal(t, "open", uses[0].State)

					var current model.NativeTask
					require.NoError(t, db.First(&current, "id = ?", first.ID).Error)
					require.Equal(t, "reserved", current.Status)
				}
			})
		}
	}
}
