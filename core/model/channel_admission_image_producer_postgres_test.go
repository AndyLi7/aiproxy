package model_test

import (
	"context"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestPostgresImageProducerLockBoundaries(t *testing.T) {
	for _, action := range []string{"receipt-log", "receipt-journal", "return", "release"} {
		for _, outcome := range []string{"live", "changed", "cancelled"} {
			t.Run(action+"/"+outcome, func(t *testing.T) {
				db := queueLockPostgres(t)
				f := runtimeAdmissionOnFixture(t, admissionFixtureOn(t, db, ""))
				installImageProducer(t, db)
				task, usage := producerFixture(t, f)
				if action != "release" {
					prepareProducerAttempt(t, f, task)
				}
				if action == "return" {
					require.NoError(t, model.AcceptSubmittedImageTask(t.Context(), task, "upstream"))
				}
				before := producerRows(t, db)
				root := before[0]
				pool, e := db.DB()
				require.NoError(t, e)
				holder, e := pool.BeginTx(t.Context(), nil)
				require.NoError(t, e)
				defer holder.Rollback()
				if action == "receipt-log" {
					var id int
					require.NoError(t, holder.QueryRowContext(t.Context(), "SELECT id FROM logs WHERE id=$1 FOR UPDATE", usage.LogID).Scan(&id))
				} else {
					var id string
					require.NoError(t, holder.QueryRowContext(t.Context(), "SELECT id FROM channel_admission_image_producers WHERE id=$1 FOR UPDATE", root.ID).Scan(&id))
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					switch action {
					case "receipt-log", "receipt-journal":
						done <- model.AcceptSubmittedImageTask(ctx, task, "upstream")
					case "return":
						done <- model.RecordImageProducerReturn(ctx, task, false)
					case "release":
						done <- model.ReleaseImageTaskReservationGenerationContext(ctx, task)
					}
				}()
				var app string
				require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&app).Error)
				require.Eventually(t, func() bool {
					var n int64
					e := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).Scan(&n).Error
					return e == nil && n > 0
				}, 2*time.Second, 5*time.Millisecond)
				if outcome == "changed" {
					if action == "receipt-log" {
						_, e = holder.ExecContext(t.Context(), "UPDATE logs SET billing_operation_id='changed' WHERE id=$1", usage.LogID)
					} else {
						_, e = holder.ExecContext(t.Context(), "UPDATE channel_admission_image_producers SET authorization_hash='changed' WHERE id=$1", root.ID)
					}
					require.NoError(t, e)
				}
				if outcome == "cancelled" {
					cancel()
					select {
					case e = <-done:
						require.Error(t, e)
					case <-time.After(time.Second):
						t.Fatal("cancelled producer transaction remained blocked")
					}
				}
				require.NoError(t, holder.Commit())
				if outcome != "cancelled" {
					select {
					case e = <-done:
					case <-time.After(3 * time.Second):
						t.Fatal("producer transaction did not return")
					}
					if outcome == "live" {
						require.NoError(t, e)
					} else {
						require.Error(t, e)
					}
				}
				var current model.ImageTask
				e = db.First(&current, "id=?", task.ID).Error
				if action == "release" && outcome == "live" {
					require.Error(t, e)
					require.Contains(t, producerRows(t, db)[0].ReceiptJSON, "reservation_released")
					return
				}
				require.NoError(t, e)
				if outcome != "live" {
					for _, row := range producerRows(t, db) {
						require.Equal(t, "active", row.State)
						require.Zero(t, row.ReturnedAtMs)
					}
					if action != "return" {
						require.Equal(t, "submitting", current.Status)
						var u model.AsyncUsageInfo
						require.NoError(t, db.First(&u, usage.ID).Error)
						require.Equal(t, model.AsyncUsageStatusNone, u.Status)
					}
				}
			})
		}
	}
}
