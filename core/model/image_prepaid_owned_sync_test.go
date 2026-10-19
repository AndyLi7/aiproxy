package model_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

//nolint:gocyclo // The transaction-lock matrix keeps each injected change beside its corresponding accounting and ownership assertions.
func TestPostgresPrepaidOwnedSyncLocks(t *testing.T) {
	for _, operation := range []string{"terminal", "timeout", "success", "receipt"} {
		for _, scenario := range []string{"task-live", "task-expired", "task-owner", "log-live", "log-expired", "sidecar-live", "sidecar-expired"} {
			if operation == "success" &&
				(scenario == "log-live" || scenario == "log-expired" || scenario == "sidecar-live" || scenario == "sidecar-expired") {
				continue
			}

			if operation == "receipt" &&
				(scenario == "sidecar-live" || scenario == "sidecar-expired") {
				continue
			}

			t.Run(operation+"/"+scenario, func(t *testing.T) {
				db := queueLockPostgres(t)
				f := runtimeAdmissionOnFixture(t, admissionFixtureOn(t, db, ""))
				installImageRecoveryHandoff(t, db)
				task := imageRecoveryTask(t, f)

				initialStatus := "queued"
				if operation == "success" || operation == "receipt" {
					initialStatus = "completed"
				}

				require.NoError(
					t,
					db.Model(&model.ImageTask{}).
						Where("id=?", task.ID).
						Updates(map[string]any{"status": initialStatus, "upstream_id": "test-upstream", "queued_at": time.Now().Add(-model.AsyncGenerationDeadline - time.Minute)}).
						Error,
				)
				task, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
				require.NoError(t, err)

				var info model.AsyncUsageInfo
				require.NoError(t, db.First(&info, task.UsageID).Error)

				deadline := time.Now().Add(1500 * time.Millisecond)
				require.NoError(
					t,
					db.Model(&model.AsyncUsageInfo{}).
						Where("id=?", info.ID).
						Updates(map[string]any{"status": model.AsyncUsageStatusPending, "processing_token": "poll-original", "next_poll_at": deadline, "billing_operation_id": task.BillingOperationID}).
						Error,
				)
				require.NoError(t, db.First(&info, info.ID).Error)

				if operation == "receipt" {
					require.NoError(
						t,
						db.Model(&model.ImageTask{}).
							Where("id=?", task.ID).
							Updates(map[string]any{"prepayment_quote_json": `{"version":1,"quoteVersion":"receipt-test","currency":"USD","prepaidMicros":100,"routes":[{"routeId":"primary","channelId":1,"provider":"fal","endpoint":"synthetic","credentialScope":"local-fixture","estimatedMicros":100,"prepaidMicros":100,"rule":{"mode":"list_ratio","ratio":"1"}}]}`, "data": `[{"url":"https://example.invalid/out.png"}]`, "billing": `{"currency":"USD","status":"settled","prepaid_micros":100,"actual_micros":50,"refund_micros":50}`, "billing_settled": true}).
							Error,
					)
					require.NoError(
						t,
						db.Model(&model.Log{}).
							Where("id=?", info.LogID).
							Update("billing_operation_id", task.BillingOperationID).
							Error,
					)
					task, err = model.GetImageTask(task.ID, task.GroupID, task.TokenID)
					require.NoError(t, err)
				}

				sidecar := scenario == "sidecar-live" || scenario == "sidecar-expired"
				if sidecar {
					won, err := model.ClaimImagePrepaymentRecovery(t.Context(), task, time.Now())
					require.NoError(t, err)
					require.True(t, won)
					require.NoError(t, model.ReleaseImagePrepaymentRecovery(t.Context(), task))
					task, err = model.GetImageTask(task.ID, task.GroupID, task.TokenID)
					require.NoError(t, err)
				}

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

				switch {
				case sidecar:
					var id string
					require.NoError(
						t,
						holder.QueryRowContext(ctx, "SELECT id FROM channel_admission_image_recovery_claims LIMIT 1 FOR UPDATE").
							Scan(&id),
					)
				case scenario == "log-live" || scenario == "log-expired":
					var id int
					require.NoError(
						t,
						holder.QueryRowContext(ctx, "SELECT id FROM logs WHERE id=$1 FOR UPDATE", info.LogID).
							Scan(&id),
					)
				default:
					var id string
					require.NoError(
						t,
						holder.QueryRowContext(ctx, "SELECT id FROM image_tasks WHERE id=$1 FOR UPDATE", task.ID).
							Scan(&id),
					)
				}

				type result struct {
					err error
					won bool
				}

				done := make(chan result, 1)
				go func() {
					var r result
					switch operation {
					case "terminal":
						r.err = model.SetClaimedImageTaskResult(
							ctx,
							&info,
							task,
							"failed",
							nil,
							&model.ImageTaskError{Code: "upstream_failed"},
						)
						r.won = r.err == nil
					case "timeout":
						r.won, r.err = model.ExpireImageGeneration(task.ID, time.Now(), &info)
					case "receipt":
						r.err = model.CompleteClaimedPrepaidImageUsage(ctx, &info, task)
						r.won = r.err == nil
					case "success":
						r.won, r.err = model.ClaimImagePrepaymentFromPoll(ctx, &info, task)
					}

					done <- r
				}()

				var app string
				require.NoError(
					t,
					db.Raw("SELECT current_setting('application_name')").Scan(&app).Error,
				)
				require.Eventually(t, func() bool {
					var n int64

					e := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).
						Scan(&n).
						Error

					return e == nil && n > 0
				}, time.Second, 5*time.Millisecond)

				if scenario == "task-owner" {
					_, err = holder.ExecContext(
						ctx,
						"UPDATE async_usage_infos SET processing_token='next-owner' WHERE id=$1",
						info.ID,
					)
					require.NoError(t, err)
				}

				expired := scenario == "task-expired" || scenario == "log-expired" ||
					scenario == "sidecar-expired"
				if expired {
					time.Sleep(time.Until(deadline.Add(30 * time.Millisecond)))
				}

				require.NoError(t, holder.Commit())

				r := <-done

				var usage model.AsyncUsageInfo
				require.NoError(t, db.First(&usage, info.ID).Error)
				fresh, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
				require.NoError(t, err)

				var entry model.Log
				require.NoError(t, db.First(&entry, info.LogID).Error)

				if expired || scenario == "task-owner" {
					require.True(t, r.err != nil || !r.won)
					require.Equal(t, initialStatus, fresh.Status)
					require.Equal(t, model.AsyncUsageStatusPending, usage.Status)
					require.Equal(t, model.AsyncUsageStatusPending, entry.AsyncUsageStatus)
					require.Nil(t, model.ImageTerminalWalletTask(&info, fresh))

					rows := imageClaimRows(t, db)
					if sidecar {
						require.Len(t, rows, 1)
						require.Empty(t, rows[0].Owner)
					} else {
						require.Empty(t, rows)
					}
				} else {
					require.NoError(t, r.err)
					require.True(t, r.won)

					if operation == "receipt" {
						require.Equal(t, model.AsyncUsageStatusCompleted, usage.Status)
						require.Equal(t, model.AsyncUsageStatusCompleted, entry.AsyncUsageStatus)
						require.Equal(t, 0.00005, entry.Amount.UsedAmount)
						require.Empty(t, imageClaimRows(t, db))

						return
					}

					owned := task
					if operation != "success" {
						require.Equal(t, "failed", fresh.Status)
						require.Equal(t, model.AsyncUsageStatusFailed, usage.Status)

						owned = model.ImageTerminalWalletTask(&info, fresh)
					}

					require.NotNil(t, owned)
					require.True(t, model.ImagePrepaymentRecoveryOwned(owned))
					require.NoError(t, model.ValidateImagePrepaymentRecovery(ctx, owned))
					require.NoError(t, model.ReleaseImagePrepaymentRecovery(ctx, owned))
				}
			})
		}
	}
}
