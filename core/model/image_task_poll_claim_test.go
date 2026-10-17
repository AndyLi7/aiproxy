package model_test

import (
	"context"
	"encoding/json"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"testing"
	"time"
)

func pollReceipt(t *testing.T, op string, info *model.AsyncUsageInfo, task *model.ImageTask) error {
	t.Helper()
	switch op {
	case "result":
		return model.SetClaimedImageTaskResult(t.Context(), info, task, "completed", []model.ImageOutput{{URL: "https://cdn.example/result.png"}}, nil, model.ImageResultMetadata{Prompt: "provider prompt"})
	case "usage-fail":
		return model.FailClaimedImageTaskUsage(t.Context(), info, task, "invalid stored count")
	case "fail":
		return model.SetClaimedImageTaskResult(t.Context(), info, task, "failed", nil, &model.ImageTaskError{Code: "archive_size_exceeded"})
	case "save":
		return model.SaveClaimedArchivedImage(t.Context(), info, task, 0, model.ImageOutput{URL: "https://owned.example/result.png", Stored: true})
	case "archive":
		return model.CompleteClaimedImageArchive(t.Context(), info, task)
	default:
		return model.ValidateClaimedImageTask(t.Context(), info, task)
	}
}
func pollFixture(t *testing.T, db *gorm.DB, op string, deadline time.Time) (*model.AsyncUsageInfo, *model.ImageTask) {
	info := claimDeadlineFixture(t, db, deadline)
	if op == "save" || op == "archive" {
		encoded, err := json.Marshal([]model.ImageOutput{{URL: "https://cdn.example/input.png", Stored: true}})
		require.NoError(t, err)
		require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", info.ImageTaskID).Updates(map[string]any{"status": "result_processing", "data": string(encoded)}).Error)
	}
	task, err := model.GetImageTask(info.ImageTaskID, info.GroupID, info.TokenID)
	require.NoError(t, err)
	return info, task
}
func TestImagePollReceiptFencing(t *testing.T) {
	for _, op := range []string{"result", "fail", "usage-fail", "save", "archive", "check"} {
		t.Run(op, func(t *testing.T) {
			for _, mode := range []string{"live", "expired", "owner", "not-pending", "renewed", "frozen", "after-write"} {
				t.Run(mode, func(t *testing.T) {
					db := asyncRenewalDB(t)
					deadline := time.Now().Add(time.Second).Truncate(time.Millisecond)
					info, task := pollFixture(t, db, op, deadline)
					beforeData := model.CloneImageOutputs(task.Data)
					switch mode {
					case "expired":
						require.NoError(t, db.Model(info).Update("next_poll_at", time.Now().Add(-time.Second)).Error)
					case "owner":
						require.NoError(t, db.Model(info).Update("processing_token", "replacement").Error)
						info.ProcessingToken = "original"
					case "not-pending":
						require.NoError(t, db.Model(info).Update("status", model.AsyncUsageStatusCompleted).Error)
					case "renewed":
						require.NoError(t, db.Model(info).Update("next_poll_at", deadline.Add(time.Minute)).Error)
						info.NextPollAt = time.Now().Add(-time.Second)
					case "frozen":
						require.NoError(t, db.Model(task).Update("validation_contract", `{"changed":true}`).Error)
						task.ValidationContract = ""
					case "after-write":
						if op != "check" {
							require.NoError(t, db.Callback().Update().After("gorm:update").Register("test:expiry", func(tx *gorm.DB) { time.Sleep(time.Until(deadline.Add(20 * time.Millisecond))) }))
							defer db.Callback().Update().Remove("test:expiry")
						}
					}
					err := pollReceipt(t, op, info, task)
					success := mode == "live" || mode == "renewed" || (op == "check" && mode == "after-write")
					if success {
						require.NoError(t, err)
					} else {
						require.ErrorIs(t, err, model.ErrImagePollClaimLost)
					}
					saved, e := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
					require.NoError(t, e)
					if !success {
						require.Equal(t, task.Status, saved.Status)
						require.Equal(t, beforeData, saved.Data)
						require.Equal(t, beforeData, task.Data)
					}
					if success && op == "fail" {
						var out model.AsyncUsageInfo
						require.NoError(t, db.First(&out, info.ID).Error)
						require.Empty(t, out.ProcessingToken)
						require.Equal(t, model.AsyncUsageStatusFailed, out.Status)
					}
					if success && op == "result" {
						require.Equal(t, "provider prompt", saved.Prompt)
					}
				})
			}
		})
	}
}
func TestImagePollFailureLogIdentityAndGrace(t *testing.T) {
	for _, mode := range []string{"missing", "wrong-owner", "wrong-operation", "completed"} {
		t.Run(mode, func(t *testing.T) {
			db := asyncRenewalDB(t)
			info, task := pollFixture(t, db, "fail", time.Now().Add(time.Minute))
			query := db.Model(&model.Log{}).Where("id=?", info.LogID)
			switch mode {
			case "missing":
				require.NoError(t, query.Delete(&model.Log{}).Error)
			case "wrong-owner":
				require.NoError(t, query.Update("token_id", 42).Error)
			case "wrong-operation":
				require.NoError(t, query.Update("billing_operation_id", "other").Error)
			case "completed":
				require.NoError(t, query.Update("async_usage_status", model.AsyncUsageStatusCompleted).Error)
			}
			err := pollReceipt(t, "fail", info, task)
			if mode == "missing" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, model.ErrImagePollClaimLost)
				saved, e := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
				require.NoError(t, e)
				require.Equal(t, "queued", saved.Status)
			}
		})
	}
}
func TestImagePollArchiveStaleDataCAS(t *testing.T) {
	for _, op := range []string{"save", "archive"} {
		t.Run(op, func(t *testing.T) {
			db := asyncRenewalDB(t)
			info, task := pollFixture(t, db, op, time.Now().Add(time.Minute))
			require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("data", `[{"url":"https://other.example/changed.png","stored":true}]`).Error)
			require.Error(t, pollReceipt(t, op, info, task))
			saved, e := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
			require.NoError(t, e)
			require.Equal(t, "https://other.example/changed.png", saved.Data[0].URL)
			require.Equal(t, "result_processing", saved.Status)
		})
	}
}
func TestPostgresImagePollReceiptLocks(t *testing.T) {
	for _, op := range []string{"result", "save", "archive", "fail"} {
		t.Run(op, func(t *testing.T) {
			modes := []string{"live", "expired-task-lock", "renewed", "owner"}
			if op == "fail" {
				modes = append(modes, "last-log-lock")
			}
			for _, mode := range modes {
				t.Run(mode, func(t *testing.T) {
					if mode == "last-log-lock" && op != "fail" {
						return
					}
					db := queueLockPostgres(t)
					old := model.LogDB
					model.LogDB = db
					t.Cleanup(func() { model.LogDB = old })
					deadline := time.Now().Add(1500 * time.Millisecond).Truncate(time.Microsecond)
					info, task := pollFixture(t, db, op, deadline)
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					pool, e := db.DB()
					require.NoError(t, e)
					holder, e := pool.BeginTx(ctx, nil)
					require.NoError(t, e)
					defer holder.Rollback()
					if mode == "last-log-lock" {
						var id int
						require.NoError(t, holder.QueryRowContext(ctx, "SELECT id FROM logs WHERE id=$1 FOR UPDATE", info.LogID).Scan(&id))
					} else {
						var id string
						require.NoError(t, holder.QueryRowContext(ctx, "SELECT id FROM image_tasks WHERE id=$1 FOR UPDATE", task.ID).Scan(&id))
					}
					done := make(chan error, 1)
					go func() { done <- pollReceipt(t, op, info, task) }()
					var app string
					require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&app).Error)
					require.Eventually(t, func() bool {
						var n int64
						e := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).Scan(&n).Error
						return e == nil && n > 0
					}, time.Second, 5*time.Millisecond)
					switch mode {
					case "expired-task-lock", "last-log-lock":
						time.Sleep(time.Until(deadline.Add(20 * time.Millisecond)))
					case "renewed":
						_, e = holder.ExecContext(ctx, "UPDATE async_usage_infos SET next_poll_at=$1 WHERE id=$2", deadline.Add(time.Minute), info.ID)
						require.NoError(t, e)
						time.Sleep(time.Until(deadline.Add(20 * time.Millisecond)))
					case "owner":
						_, e = holder.ExecContext(ctx, "UPDATE async_usage_infos SET processing_token='replacement' WHERE id=$1", info.ID)
						require.NoError(t, e)
					}
					require.NoError(t, holder.Commit())
					err := <-done
					if mode == "live" || mode == "renewed" {
						require.NoError(t, err)
					} else {
						require.ErrorIs(t, err, model.ErrImagePollClaimLost)
						saved, e := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
						require.NoError(t, e)
						require.Equal(t, task.Status, saved.Status)
						var usage model.AsyncUsageInfo
						require.NoError(t, db.First(&usage, info.ID).Error)
						require.Equal(t, model.AsyncUsageStatusPending, usage.Status)
						var entry model.Log
						require.NoError(t, db.First(&entry, info.LogID).Error)
						require.Equal(t, model.AsyncUsageStatusPending, entry.AsyncUsageStatus)
					}
				})
			}
		})
	}
}
