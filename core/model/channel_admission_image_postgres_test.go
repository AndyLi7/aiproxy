package model_test

import (
	"context"
	"strings"
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

// Activation happens after the legacy preparation and before claimed:true is
// persisted. Row-lock waits must re-evaluate the exact original qualification.
func TestPostgresAdmissionImageActivationAfterPreparation(t *testing.T) {
	for _, outcome := range []string{"live", "sealed", "changed", "expired", "lost-process-proof"} {
		t.Run(outcome, func(t *testing.T) {
			db := queueLockPostgres(t)
			f := runtimeAdmissionOnFixture(t, admissionFixtureOn(t, db, ""))
			require.NoError(t, db.AutoMigrate(&model.Log{}))
			require.NoError(t, db.Where("id = ?", f.proof.ProofID).Delete(&model.ChannelAdmissionProof{}).Error)
			require.NoError(t, db.Model(&model.ChannelAdmissionControl{}).Where("id = ?", 1).Updates(map[string]any{"epoch": "", "epoch_sequence": 0, "storage_generation": "", "membership_hash": "", "executor_fence_hash": "", "active_proof_id": ""}).Error)
			channel, err := model.GetChannelByID(1)
			require.NoError(t, err)
			task, created, err := model.ReserveImageTask(&model.ImageTask{ID: "pre-c", Model: "synthetic/image", GroupID: "g", TokenID: 1, Fingerprint: strings.Repeat("a", 64), BillingOperationID: "bill", KeyFingerprint: f.identities[0].KeyDigest, ChannelType: channel.Type, UpstreamModel: "synthetic"}, &model.AsyncUsageInfo{RequestID: "pre-c", ChannelID: 1, BaseURL: channel.BaseURL, GroupID: "g", TokenID: 1})
			require.NoError(t, err)
			require.True(t, created)
			require.Empty(t, task.AdmissionGeneration)
			deadline := time.Now().Add(1200 * time.Millisecond)
			if outcome == "expired" {
				task.CreatedAt = deadline.Add(-model.AsyncGenerationDeadline)
				require.NoError(t, db.Model(&model.ImageTask{}).Where("id = ?", task.ID).UpdateColumn("created_at", task.CreatedAt).Error)
			}
			attempt, err := model.PrepareImageTaskAttempt(context.Background(), task, f.identities[0], 0)
			require.NoError(t, err)
			require.Empty(t, attempt)
			if outcome == "lost-process-proof" {
				var reloaded model.ImageTask
				require.NoError(t, db.First(&reloaded, "id = ?", task.ID).Error)
				task = &reloaded
			}
			_, err = f.store.Activate(context.Background(), 0)
			require.NoError(t, err)
			if outcome == "sealed" {
				_, err = f.store.Seal(context.Background(), f.seal("move", 0, 1, 2, ""))
				require.NoError(t, err)
			}
			task.Attempts = []model.ImageTaskAttempt{{ChannelID: 1, Failure: failover.Failure{Acceptance: failover.Unknown}}}
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
			go func() { done <- model.SaveImageTaskAttempt(task, 0, 1, channel.BaseURL) }()
			var application string
			require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&application).Error)
			require.Eventually(t, func() bool {
				var count int64
				e := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", application).Scan(&count).Error
				return e == nil && count > 0
			}, 2*time.Second, 5*time.Millisecond)
			if outcome == "changed" {
				_, err = holder.ExecContext(ctx, "UPDATE image_tasks SET billing_operation_id=$1 WHERE id=$2", "replacement", task.ID)
				require.NoError(t, err)
			}
			if outcome == "expired" {
				if delay := time.Until(deadline.Add(50 * time.Millisecond)); delay > 0 {
					time.Sleep(delay)
				}
			}
			require.NoError(t, holder.Commit())
			err = <-done
			var current model.ImageTask
			require.NoError(t, db.First(&current, "id = ?", task.ID).Error)
			if outcome == "live" || outcome == "sealed" {
				require.NoError(t, err)
				require.Len(t, current.Attempts, 1)
				require.Len(t, runtimeUses(t, db), 2)
				require.Error(t, model.SaveImageTaskAttempt(task, 0, 1, channel.BaseURL), "first claim must not replay")
			} else {
				require.Error(t, err)
				require.Empty(t, current.Attempts)
				require.Empty(t, runtimeUses(t, db))
			}
		})
	}
}
