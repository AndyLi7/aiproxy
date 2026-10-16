package model_test

import (
	"context"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

// Each holder is an independent SQL transaction/connection. Confirm actual
// PostgreSQL lock waits before releasing it; no simulated SQLite row locks.
func TestPostgresImageRecoveryHandoffLocks(t *testing.T) {
	for _, phase := range []string{"prepare-task", "prepare-proof", "accept-journal", "accept-proof", "exit-journal", "exit-proof"} {
		for _, outcome := range []string{"live", "expired", "mutated"} {
			t.Run(phase+"/"+outcome, func(t *testing.T) {
				db := queueLockPostgres(t)
				f := runtimeAdmissionOnFixture(t, admissionFixtureOn(t, db, ""))
				installImageRecoveryHandoff(t, db)
				task := claimedImageHandoffTask(t, f)
				deadline := time.Now().Add(1600 * time.Millisecond)
				require.NoError(t, model.ShortenImageRecoveryDeadlineForTest(task, deadline))
				var h *model.ImageRecoveryHandoffHandle
				var err error
				if phase != "prepare-task" && phase != "prepare-proof" {
					h, err = model.PrepareImageRecoveryHandoff(t.Context(), task)
					require.NoError(t, err)
					require.NotNil(t, h)
				}
				if phase == "exit-journal" || phase == "exit-proof" {
					ok, e := h.Accept(t.Context())
					require.NoError(t, e)
					require.True(t, ok)
				}
				before := imageRecoveryHandoffs(t, db)
				claim := imageClaimRows(t, db)[0]
				pool, err := db.DB()
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				holder, err := pool.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer holder.Rollback()
				table, key := "image_tasks", task.ID
				if phase == "prepare-proof" || phase == "accept-proof" || phase == "exit-proof" {
					table, key = "channel_admission_proofs", f.proof.ProofID
				}
				if phase == "accept-journal" || phase == "exit-journal" {
					table, key = "channel_admission_image_recovery_handoffs", before[0].ID
				}
				var id string
				require.NoError(t, holder.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE id=$1 FOR UPDATE", key).Scan(&id))
				done := make(chan error, 1)
				go func() {
					var e error
					switch phase {
					case "prepare-task", "prepare-proof":
						_, e = model.PrepareImageRecoveryHandoff(ctx, task)
					case "accept-journal", "accept-proof":
						_, e = h.Accept(ctx)
					default:
						e = h.ReleaseAndRecordExit(ctx, false)
					}
					done <- e
				}()
				var app string
				require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&app).Error)
				require.Eventually(t, func() bool {
					var n int64
					e := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).Scan(&n).Error
					return e == nil && n > 0
				}, time.Second, 5*time.Millisecond)
				if outcome == "expired" {
					if delay := time.Until(deadline.Add(30 * time.Millisecond)); delay > 0 {
						time.Sleep(delay)
					}
				}
				if outcome == "mutated" {
					switch table {
					case "image_tasks":
						_, err = holder.ExecContext(ctx, "UPDATE image_tasks SET created_at=created_at+interval '1 second' WHERE id=$1", key)
					case "channel_admission_proofs":
						_, err = holder.ExecContext(ctx, "UPDATE channel_admission_proofs SET revoked=true WHERE id=$1", key)
					default:
						_, err = holder.ExecContext(ctx, "UPDATE channel_admission_image_recovery_handoffs SET authorization_hash='corrupt' WHERE id=$1", key)
					}
					require.NoError(t, err)
				}
				require.NoError(t, holder.Commit())
				err = <-done
				after := imageRecoveryHandoffs(t, db)
				if outcome == "live" {
					require.NoError(t, err)
					require.Len(t, after, 1)
					if phase == "exit-journal" || phase == "exit-proof" {
						require.Equal(t, "exited", after[0].State)
						require.Empty(t, imageClaimRows(t, db)[0].Owner)
					}
				} else {
					require.Error(t, err)
					require.Equal(t, claim, imageClaimRows(t, db)[0])
					require.Len(t, after, len(before))
					if len(after) > 0 {
						require.Equal(t, before[0].State, after[0].State)
						require.Equal(t, "active", after[0].ExecutionState)
						require.Zero(t, after[0].ExitedAtMs)
					}
				}
			})
		}
	}
}

func TestPostgresImageRecoveryHandoffExpiryAfterWrites(t *testing.T) {
	assertImageRecoveryHandoffExpiryDuringWrites(t, func(t *testing.T) *admissionFixture {
		return runtimeAdmissionOnFixture(t, admissionFixtureOn(t, queueLockPostgres(t), ""))
	})
}
