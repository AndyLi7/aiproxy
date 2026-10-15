package model_test

import (
	"context"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestPostgresAdmissionImageRechecksAfterTaskLock(t *testing.T) {
	for _, action := range []string{"prepare", "save", "release", "billing"} {
		outcomes := []string{"changed", "live"}
		if action == "prepare" || action == "save" {
			outcomes = append(outcomes, "expired")
		}
		for _, outcome := range outcomes {
			t.Run(action+"/"+outcome, func(t *testing.T) {
				db := queueLockPostgres(t)
				f := runtimeAdmissionOnFixture(t, admissionFixtureOn(t, db, ""))
				task, _ := reserveAdmissionImage(t, f, "locked", 0)
				channel, err := model.GetChannelByID(1)
				require.NoError(t, err)
				if action == "save" {
					task.AdmissionAttempt, err = model.PrepareImageTaskAttempt(context.Background(), task, f.identities[0], 0)
					require.NoError(t, err)
					task.Attempts = []model.ImageTaskAttempt{{ChannelID: 1, Failure: failover.Failure{Acceptance: failover.Unknown}}}
				}
				if action == "billing" {
					task.Billing = &model.ImageTaskBilling{Currency: "USD", Status: "pending"}
				}
				deadline := time.Now().Add(700 * time.Millisecond)
				if outcome == "expired" {
					task.CreatedAt = deadline.Add(-model.AsyncGenerationDeadline)
					require.NoError(t, db.Model(&model.ImageTask{}).Where("id = ?", task.ID).UpdateColumn("created_at", task.CreatedAt).Error)
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
					switch action {
					case "prepare":
						_, e := model.PrepareImageTaskAttempt(ctx, task, f.identities[0], 0)
						done <- e
					case "save":
						done <- model.SaveImageTaskAttempt(task, 0, 1, channel.BaseURL)
					case "release":
						done <- model.ReleaseImageTaskReservationGeneration(task)
					case "billing":
						done <- model.SaveImageTaskBilling(task)
					}
				}()
				var application string
				require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&application).Error)
				require.Eventually(t, func() bool {
					var count int64
					err := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", application).Scan(&count).Error
					return err == nil && count > 0
				}, 2*time.Second, 5*time.Millisecond)
				if outcome == "changed" {
					_, err = holder.ExecContext(ctx, "UPDATE image_tasks SET billing_operation_id=$1 WHERE id=$2", "replacement-generation", task.ID)
					require.NoError(t, err)
				}
				if outcome == "expired" {
					if delay := time.Until(deadline.Add(50 * time.Millisecond)); delay > 0 {
						time.Sleep(delay)
					}
				}
				require.NoError(t, holder.Commit())
				err = <-done
				if outcome == "live" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				var current model.ImageTask
				read := db.First(&current, "id = ?", task.ID).Error
				if action == "release" && outcome == "live" {
					require.Error(t, read)
					require.Equal(t, "closed", runtimeUses(t, db)[0].State)
					return
				}
				require.NoError(t, read)
				if outcome != "live" {
					require.Equal(t, "submitting", current.Status)
					require.Empty(t, current.Attempts)
					require.Nil(t, current.Billing)
				}
				uses := runtimeUses(t, db)
				want := 1
				if action == "save" || (action == "prepare" && outcome == "live") {
					want = 2
				}
				require.Len(t, uses, want)
				for _, use := range uses {
					require.Equal(t, "open", use.State)
				}
			})
		}
	}
}
