package model_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func submissionReceiptFixture(t *testing.T, db *gorm.DB, age ...time.Duration) (*model.ImageTask, *model.AsyncUsageInfo) {
	t.Helper()
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))
	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })
	ch := &model.Channel{ID: 7, Type: model.ChannelTypeFal, Key: "synthetic-local", BaseURL: "https://provider.invalid"}
	identity, e := model.ChannelIdentityOf(ch)
	require.NoError(t, e)
	task := &model.ImageTask{ID: "owned-submit", GroupID: "g", TokenID: 1, Model: "image", Fingerprint: "original-fingerprint", BillingOperationID: "original-operation", KeyFingerprint: identity.KeyDigest, ChannelType: ch.Type, UpstreamModel: "original-model", ExpectedImages: 1}
	info := &model.AsyncUsageInfo{RequestID: task.ID, GroupID: task.GroupID, TokenID: task.TokenID, BillingOperationID: task.BillingOperationID, ChannelID: ch.ID, BaseURL: ch.BaseURL, Price: model.Price{PerRequestPrice: .37}, PricingVersion: "frozen-v1"}
	saved, created, e := model.ReserveImageTask(task, info)
	require.NoError(t, e)
	require.True(t, created)
	if len(age) > 0 {
		saved.CreatedAt = time.Now().UTC().Add(-age[0])
		require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", saved.ID).Update("created_at", saved.CreatedAt).Error)
	}
	if db.Dialector.Name() == "postgres" {
		var persisted model.ImageTask
		require.NoError(t, db.First(&persisted, "id=?", saved.ID).Error)
		// Keep the fixture snapshot in the database dialect's stored representation.
		saved.CreatedAt = persisted.CreatedAt
	}
	attempt, e := model.PrepareImageTaskAttempt(context.Background(), saved, identity, 0)
	require.NoError(t, e)
	saved.AdmissionAttempt = attempt
	saved.Attempts = []model.ImageTaskAttempt{{ChannelID: ch.ID, StartedAt: time.Now(), Decision: "in_flight"}}
	require.NoError(t, model.SaveImageTaskAttempt(saved, 0, ch.ID, ch.BaseURL))
	return saved, info
}

func TestImageSubmissionReceiptsBindOriginalActor(t *testing.T) {
	for _, receipt := range []string{"accept", "complete", "failed", "unknown"} {
		for _, change := range []string{"valid", "terminal", "operation", "price", "attempt", "quote", "usage", "log", "read-copy", "cancelled"} {
			t.Run(receipt+"/"+change, func(t *testing.T) {
				db, e := model.OpenSQLite(filepath.Join(t.TempDir(), "submit.db"))
				require.NoError(t, e)
				pool, e := db.DB()
				require.NoError(t, e)
				t.Cleanup(func() { require.NoError(t, pool.Close()) })
				task, info := submissionReceiptFixture(t, db)
				switch change {
				case "terminal":
					require.NoError(t, model.SetImageTaskResult(task.ID, "failed", nil, &model.ImageTaskError{Code: "submission_rejected"}))
				case "operation":
					require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("billing_operation_id", "different").Error)
				case "price":
					require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("per_request_price", 9).Error)
				case "attempt":
					require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("attempts", `[{"channel_id":7,"decision":"different"}]`).Error)
				case "quote":
					require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("prepayment_quote_json", `{"changed":true}`).Error)
				case "usage":
					require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("token_id", 2).Error)
				case "log":
					require.NoError(t, db.Model(&model.Log{}).Where("id=?", info.LogID).Update("billing_operation_id", "different").Error)
				case "read-copy":
					task, e = model.GetImageTask(task.ID, task.GroupID, task.TokenID)
					require.NoError(t, e)
				}
				var beforeTask model.ImageTask
				var beforeUsage model.AsyncUsageInfo
				var beforeLog model.Log
				require.NoError(t, db.First(&beforeTask, "id=?", task.ID).Error)
				require.NoError(t, db.First(&beforeUsage, info.ID).Error)
				require.NoError(t, db.First(&beforeLog, info.LogID).Error)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if change == "cancelled" {
					cancel()
				}
				metadata := model.ImageResultMetadata{ProviderMetadata: map[string]json.RawMessage{"unknown": json.RawMessage(`{"preserved":true}`)}, SeedExact: "9007199254740993", BillableUnits: "1.25"}
				switch receipt {
				case "accept":
					e = model.AcceptSubmittedImageTask(ctx, task, "provider-job")
				case "complete":
					e = model.CompleteSubmittedImageTask(ctx, task, []model.ImageOutput{{URL: "https://example.invalid/image.png", AuxiliaryImages: map[string]*model.ImageOutput{"mask": {URL: "https://example.invalid/mask.png"}}}}, metadata)
				case "failed":
					e = model.SetSubmittedImageTaskResult(ctx, task, "failed", &model.ImageTaskError{Code: "submission_rejected"})
				default:
					e = model.SetSubmittedImageTaskResult(ctx, task, "submission_unknown", nil)
				}
				var got model.ImageTask
				var usage model.AsyncUsageInfo
				var entry model.Log
				require.NoError(t, db.First(&got, "id=?", task.ID).Error)
				require.NoError(t, db.First(&usage, info.ID).Error)
				require.NoError(t, db.First(&entry, info.LogID).Error)
				if change != "valid" {
					require.Error(t, e)
					require.Equal(t, beforeTask, got)
					require.Equal(t, beforeUsage, usage)
					require.Equal(t, beforeLog, entry)
					return
				}
				require.NoError(t, e)
				require.Equal(t, beforeUsage.Price, usage.Price)
				require.Equal(t, "frozen-v1", usage.PricingVersion)
				require.Equal(t, "original-operation", usage.BillingOperationID)
				switch receipt {
				case "accept":
					require.Equal(t, "queued", got.Status)
					require.Equal(t, "provider-job", got.UpstreamID)
					require.Equal(t, model.AsyncUsageStatusPending, usage.Status)
				case "complete":
					require.Equal(t, "completed", got.Status)
					require.Equal(t, metadata.ProviderMetadata, got.ProviderMetadata)
					require.Equal(t, metadata.SeedExact, got.SeedExact)
					require.Equal(t, metadata.BillableUnits, got.BillableUnits)
					require.Contains(t, got.Data[0].AuxiliaryImages, "mask")
					require.Equal(t, model.AsyncUsageStatusPending, usage.Status)
				case "failed":
					require.Equal(t, "failed", got.Status)
					require.Equal(t, model.AsyncUsageStatusFailed, usage.Status)
					require.Equal(t, model.AsyncUsageStatusFailed, entry.AsyncUsageStatus)
				default:
					require.Equal(t, "submission_unknown", got.Status)
					require.Equal(t, model.AsyncUsageStatusNone, usage.Status)
				}
			})
		}
	}
}
