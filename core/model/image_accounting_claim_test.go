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

func accountingReceipt(t *testing.T, op string, info *model.AsyncUsageInfo) error {
	t.Helper()

	amount := model.Amount{
		UsedAmount:         .3,
		ImageBillingResult: &model.ImageBillingResult{State: "complete"},
	}
	switch op {
	case "measurement":
		return model.PrepareClaimedImageTaskMeasurement(
			info,
			model.Usage{ImageOutputTokens: 1},
			model.UsageContext{},
			amount,
		)
	case "park":
		return model.ParkMeasuredImageUsage(info)
	case "evidence":
		return model.UpdateAsyncUsageLogUsage(
			info,
			model.Usage{ImageOutputTokens: 1},
			model.UsageContext{},
			info.Price,
			amount,
			"USD",
			"frozen",
		)
	case "complete":
		_, err := model.CompleteClaimedAsyncUsageInfo(
			info,
			info.Usage,
			info.UsageContext,
			info.Amount,
		)

		return err
	case "fail":
		info.Error = "failed measurement"
		_, err := model.FailClaimedAsyncUsageInfo(info)
		return err
	default:
		return model.ValidateImageAccountingClaim(context.Background(), info)
	}
}

func accountingModelFixture(
	t *testing.T,
	db *gorm.DB,
	task bool,
	deadline time.Time,
) *model.AsyncUsageInfo {
	t.Helper()

	info := claimDeadlineFixture(t, db, deadline)
	if !task {
		info.ImageTaskID = ""
		info.MeasuredImage = true
		require.NoError(
			t,
			db.Model(&model.AsyncUsageInfo{}).
				Where("id=?", info.ID).
				Updates(map[string]any{"image_task_id": "", "measured_image": true}).
				Error,
		)
	}

	return info
}

func TestImageAccountingReceiptFencing(t *testing.T) {
	for _, task := range []bool{false, true} {
		name := "measured"
		if task {
			name = "task"
		}

		t.Run(name, func(t *testing.T) {
			for _, op := range []string{"park", "evidence", "complete", "fail", "check", "measurement"} {
				if !task && op == "measurement" {
					continue
				}

				t.Run(op, func(t *testing.T) {
					for _, variant := range []string{"valid", "expired", "owner", "nonpending", "renewed", "wrong-log", "after-write", "millisecond"} {
						t.Run(variant, func(t *testing.T) {
							db := asyncRenewalDB(t)

							deadline := time.Now().Add(time.Minute).Truncate(time.Millisecond)
							if variant == "after-write" {
								deadline = time.Now().
									Add(200 * time.Millisecond).
									Truncate(time.Millisecond)
							}

							info := accountingModelFixture(t, db, task, deadline)

							q := db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID)
							switch variant {
							case "expired":
								require.NoError(
									t,
									q.Update("next_poll_at", time.Now().Add(-time.Second)).Error,
								)
							case "owner":
								require.NoError(t, q.Update("processing_token", "new-owner").Error)
							case "nonpending":
								require.NoError(
									t,
									q.Update("status", model.AsyncUsageStatusCompleted).Error,
								)
							case "renewed":
								require.NoError(
									t,
									q.Update("next_poll_at", deadline.Add(time.Minute)).Error,
								)
								info.NextPollAt = time.Now().Add(-time.Hour)
							case "wrong-log":
								require.NoError(
									t,
									db.Model(&model.Log{}).
										Where("id=?", info.LogID).
										Update("token_id", 99).
										Error,
								)
							case "after-write":
								if op != "check" {
									require.NoError(
										t,
										db.Callback().
											Update().
											After("gorm:update").
											Register("test:accounting-expiry", func(*gorm.DB) { time.Sleep(time.Until(deadline.Add(10 * time.Millisecond))) }),
									)
									defer func() {
										if err := db.
											Callback().Update().
											Remove("test:accounting-expiry"); err != nil {
											t.Error(err)
										}
									}()
								}
							}

							var (
								before    model.AsyncUsageInfo
								beforeLog model.Log
							)

							require.NoError(t, db.First(&before, info.ID).Error)
							require.NoError(t, db.First(&beforeLog, info.LogID).Error)
							err := accountingReceipt(t, op, info)

							ok := variant == "valid" || variant == "renewed" ||
								variant == "millisecond" ||
								(variant == "after-write" && op == "check")
							if ok {
								require.NoError(t, err)
							} else {
								require.Error(t, err)

								var (
									after    model.AsyncUsageInfo
									afterLog model.Log
								)

								require.NoError(t, db.First(&after, info.ID).Error)
								require.NoError(t, db.First(&afterLog, info.LogID).Error)
								require.Equal(t, before, after)
								require.Equal(t, beforeLog, afterLog)
							}
						})
					}
				})
			}
		})
	}
}

func TestPostgresImageAccountingReceiptLocks(t *testing.T) {
	for _, op := range []string{"measurement", "park", "evidence", "complete", "fail"} {
		t.Run(op, func(t *testing.T) {
			for _, variant := range []string{"valid", "task-expiry", "log-expiry", "renewed", "owner", "no-task-log-expiry"} {
				if op == "measurement" && variant == "no-task-log-expiry" {
					continue
				}

				t.Run(variant, func(t *testing.T) {
					db := queueLockPostgres(t)
					old := model.LogDB
					model.LogDB = db
					t.Cleanup(func() { model.LogDB = old })

					deadline := time.Now().Add(700 * time.Millisecond).Truncate(time.Microsecond)
					info := accountingModelFixture(t, db, variant != "no-task-log-expiry", deadline)

					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()

					pool, e := db.DB()
					require.NoError(t, e)
					holder, e := pool.BeginTx(ctx, nil)
					require.NoError(t, e)

					defer func() {
						if err := holder.
							Rollback(); err !=
							nil &&
							!errors.
								Is(err, sql.ErrTxDone) {
							t.Error(err)
						}
					}()

					if variant == "log-expiry" || variant == "no-task-log-expiry" {
						var id int
						require.NoError(
							t,
							holder.QueryRowContext(ctx, "SELECT id FROM logs WHERE id=$1 FOR UPDATE", info.LogID).
								Scan(&id),
						)
					} else {
						var id string
						require.NoError(
							t,
							holder.QueryRowContext(ctx, "SELECT id FROM image_tasks WHERE id=$1 FOR UPDATE", info.ImageTaskID).
								Scan(&id),
						)
					}

					done := make(chan error, 1)
					go func() { done <- accountingReceipt(t, op, info) }()

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

					switch variant {
					case "task-expiry", "log-expiry", "no-task-log-expiry":
						time.Sleep(time.Until(deadline.Add(20 * time.Millisecond)))
					case "renewed":
						_, e = holder.ExecContext(
							ctx,
							"UPDATE async_usage_infos SET next_poll_at=$1 WHERE id=$2",
							deadline.Add(time.Minute),
							info.ID,
						)
						require.NoError(t, e)
						time.Sleep(time.Until(deadline.Add(20 * time.Millisecond)))
					case "owner":
						_, e = holder.ExecContext(
							ctx,
							"UPDATE async_usage_infos SET processing_token='new-owner' WHERE id=$1",
							info.ID,
						)
						require.NoError(t, e)
					}

					require.NoError(t, holder.Commit())

					err := <-done
					if variant == "valid" || variant == "renewed" {
						require.NoError(t, err)
					} else {
						require.Error(t, err)

						var (
							got   model.AsyncUsageInfo
							entry model.Log
						)

						require.NoError(t, db.First(&got, info.ID).Error)
						require.NoError(t, db.First(&entry, info.LogID).Error)
						require.Equal(t, model.AsyncUsageStatusPending, got.Status)
						require.Equal(t, model.AsyncUsageStatusPending, entry.AsyncUsageStatus)
						require.Zero(t, got.Amount.UsedAmount)
						require.Zero(t, entry.Amount.UsedAmount)
					}
				})
			}
		})
	}
}

func TestImageAccountingPrepaidScopeUsesPersistedBinding(t *testing.T) {
	for _, variant := range []string{"prepaid", "prepaid-operation", "measured-conflict", "wrong-task", "wrong-group", "wrong-operation", "empty-operation", "missing-task"} {
		t.Run(variant, func(t *testing.T) {
			db := asyncRenewalDB(t)
			info := accountingModelFixture(t, db, true, time.Now().Add(time.Minute))
			require.NoError(
				t,
				db.Model(&model.ImageTask{}).
					Where("id=?", info.ImageTaskID).
					Update("prepayment_quote_json", `{"frozen":true}`).
					Error,
			)
			info.BillingOperationID = "fixture-prepaid-operation"
			require.NoError(
				t,
				db.Model(&model.AsyncUsageInfo{}).
					Where("id=?", info.ID).
					Update("billing_operation_id", info.BillingOperationID).
					Error,
			)
			require.NoError(
				t,
				db.Model(&model.ImageTask{}).
					Where("id=?", info.ImageTaskID).
					Update("billing_operation_id", info.BillingOperationID).
					Error,
			)

			switch variant {
			case "prepaid-operation":
				info.BillingOperationID = "frozen-operation"
				require.NoError(
					t,
					db.Model(&model.AsyncUsageInfo{}).
						Where("id=?", info.ID).
						Update("billing_operation_id", info.BillingOperationID).
						Error,
				)
				require.NoError(
					t,
					db.Model(&model.ImageTask{}).
						Where("id=?", info.ImageTaskID).
						Update("billing_operation_id", info.BillingOperationID).
						Error,
				)
			case "measured-conflict":
				require.NoError(
					t,
					db.Model(&model.AsyncUsageInfo{}).
						Where("id=?", info.ID).
						Update("measured_image", true).
						Error,
				)
			case "wrong-task":
				info.ImageTaskID = "unbound"
			case "wrong-group":
				info.GroupID = "unbound"
			case "empty-operation":
				info.BillingOperationID = ""
				require.NoError(
					t,
					db.Model(&model.AsyncUsageInfo{}).
						Where("id=?", info.ID).
						Update("billing_operation_id", "").
						Error,
				)
				require.NoError(
					t,
					db.Model(&model.ImageTask{}).
						Where("id=?", info.ImageTaskID).
						Update("billing_operation_id", "").
						Error,
				)
			case "wrong-operation":
				require.NoError(
					t,
					db.Model(&model.ImageTask{}).
						Where("id=?", info.ImageTaskID).
						Update("billing_operation_id", "different-operation").
						Error,
				)
			case "missing-task":
				require.NoError(
					t,
					db.Where("id=?", info.ImageTaskID).Delete(&model.ImageTask{}).Error,
				)
			}

			require.Equal(
				t,
				variant != "prepaid" && variant != "prepaid-operation",
				model.UsesImageAccountingClaim(info),
			)
		})
	}
}

// Recording the affected-row behavior does not claim a live MySQL test.
func TestImageAccountingNoChangeRowCount(t *testing.T) {
	for _, op := range []string{"measurement", "evidence", "complete", "park", "fail"} {
		t.Run(op, func(t *testing.T) {
			db := asyncRenewalDB(t)
			info := accountingModelFixture(t, db, true, time.Now().Add(time.Minute))

			require.NoError(
				t,
				db.Callback().
					Update().
					After("gorm:update").
					Register("test:unchanged-count", func(tx *gorm.DB) {
						if tx.Error == nil {
							tx.RowsAffected = 0
						}
					}),
			)
			defer func() {
				if err := db.
					Callback().Update().
					Remove("test:unchanged-count"); err != nil {
					t.Error(err)
				}
			}()

			require.NoError(t, accountingReceipt(t, op, info))
		})
	}

	t.Run("suppressed-write-rejected", func(t *testing.T) {
		db := asyncRenewalDB(t)
		info := accountingModelFixture(t, db, true, time.Now().Add(time.Minute))
		require.NoError(
			t,
			db.Callback().
				Update().
				Replace("gorm:update", func(tx *gorm.DB) { tx.RowsAffected = 0 }),
		)
		require.ErrorIs(t, accountingReceipt(t, "complete", info), model.ErrImagePollClaimLost)

		var got model.AsyncUsageInfo
		require.NoError(t, db.First(&got, info.ID).Error)
		require.Equal(t, model.AsyncUsageStatusPending, got.Status)
	})
}
