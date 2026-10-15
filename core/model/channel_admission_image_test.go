package model_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func reserveAdmissionImage(t *testing.T, f *admissionFixture, id string, index int) (*model.ImageTask, *model.AsyncUsageInfo) {
	t.Helper()
	require.NoError(t, f.store.DB.AutoMigrate(&model.Log{}))
	channel, err := model.GetChannelByID(index + 1)
	require.NoError(t, err)
	task := &model.ImageTask{ID: id, Model: "synthetic/image", GroupID: "g", TokenID: 1, Fingerprint: strings.Repeat("a", 64), BillingOperationID: "billing-" + id, KeyFingerprint: f.identities[index].KeyDigest, AdmissionIdentityHash: f.identities[index].IdentityHash, ChannelType: channel.Type, UpstreamModel: "synthetic"}
	info := &model.AsyncUsageInfo{RequestID: id, ChannelID: channel.ID, BaseURL: channel.BaseURL, GroupID: "g", TokenID: 1}
	saved, created, err := model.ReserveImageTask(task, info)
	require.NoError(t, err)
	require.True(t, created)
	require.NotEmpty(t, saved.AdmissionGeneration)
	return saved, info
}
func TestAdmissionRuntimeImageReservationReleaseAndLateGeneration(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	first, _ := reserveAdmissionImage(t, f, "reused", 0)
	require.Error(t, model.ReleaseImageTaskReservation(first.ID), "ID alone cannot delete a protected generation")
	require.NoError(t, model.ReleaseImageTaskReservationGeneration(first))
	require.Equal(t, "closed", runtimeUses(t, f.store.DB)[0].State)
	second, _ := reserveAdmissionImage(t, f, "reused", 0)
	require.NotEqual(t, first.AdmissionGeneration, second.AdmissionGeneration)
	stale := *first
	stale.CreatedAt = second.CreatedAt
	stale.UsageID = second.UsageID
	require.ErrorIs(t, model.ReleaseImageTaskReservationGeneration(&stale), model.ErrChannelAdmissionConflict)
	_, err := model.PrepareImageTaskAttempt(context.Background(), &stale, f.identities[0], 0)
	require.ErrorIs(t, err, model.ErrChannelAdmissionConflict)
	require.NoError(t, model.ReleaseImageTaskReservationGeneration(second))
	_, err = f.store.Seal(context.Background(), f.seal("seal", 0, 1, 2, ""))
	require.NoError(t, err)
	_, _, err = model.ReserveImageTask(&model.ImageTask{ID: "late", Model: "m", GroupID: "g", TokenID: 1, Fingerprint: strings.Repeat("a", 64), BillingOperationID: "late", KeyFingerprint: f.identities[0].KeyDigest, AdmissionIdentityHash: f.identities[0].IdentityHash}, &model.AsyncUsageInfo{ChannelID: 1})
	require.ErrorIs(t, err, model.ErrChannelAdmissionSealed)
}
func TestAdmissionRuntimeImageFrozenFirstAttemptAndFailoverNewAdmission(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	task, _ := reserveAdmissionImage(t, f, "frozen", 0)
	_, err := f.store.Seal(context.Background(), f.seal("seal", 0, 1, 2, ""))
	require.NoError(t, err)
	attempt, err := model.PrepareImageTaskAttempt(context.Background(), task, f.identities[0], 0)
	require.NoError(t, err)
	task.AdmissionAttempt = attempt
	_, err = model.PrepareImageTaskAttempt(context.Background(), task, f.identities[0], 0)
	require.ErrorIs(t, err, model.ErrChannelAdmissionConflict)
	require.ErrorIs(t, model.ReleaseImageTaskReservationGeneration(task), model.ErrChannelAdmissionRetained)
	task.Attempts = []model.ImageTaskAttempt{{ChannelID: 1, StartedAt: time.Now(), Failure: failover.Failure{Acceptance: failover.Unknown}, Decision: "in_flight"}}
	channel, err := model.GetChannelByID(1)
	require.NoError(t, err)
	require.NoError(t, model.SaveImageTaskAttempt(task, 0, 1, channel.BaseURL))
	task.Attempts[0].Failure.Acceptance = failover.NotAccepted
	task.Attempts[0].Retry = true
	require.NoError(t, model.SaveImageTaskAttempt(task, 1, 1, channel.BaseURL))
	_, err = model.PrepareImageTaskAttempt(context.Background(), task, f.identities[0], 1)
	require.ErrorIs(t, err, model.ErrChannelAdmissionSealed)
	attempt, err = model.PrepareImageTaskAttempt(context.Background(), task, f.identities[1], 1)
	require.NoError(t, err)
	task.AdmissionAttempt = attempt
	task.Attempts = append(task.Attempts, model.ImageTaskAttempt{ChannelID: 2, StartedAt: time.Now(), Failure: failover.Failure{Acceptance: failover.Unknown}})
	next, err := model.GetChannelByID(2)
	require.NoError(t, err)
	task.KeyFingerprint = f.identities[1].KeyDigest
	require.NoError(t, model.SaveImageTaskAttempt(task, 1, 2, next.BaseURL))
	require.Len(t, runtimeUses(t, f.store.DB), 3)
}
func TestAdmissionRuntimeImageReservationAtomicityAndMissingRows(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	task, _ := reserveAdmissionImage(t, f, "missing", 0)
	require.NoError(t, f.store.DB.Delete(&model.ImageTask{}, "id = ?", task.ID).Error)
	copy := *task
	copy.UsageID = 0
	_, _, err := model.ReserveImageTask(&copy, &model.AsyncUsageInfo{ChannelID: 1})
	require.ErrorIs(t, err, model.ErrChannelAdmissionRetained)
	require.NoError(t, f.store.DB.Exec("CREATE TRIGGER reject_image_log BEFORE INSERT ON logs BEGIN SELECT RAISE(ABORT, 'synthetic log failure'); END").Error)
	copy.ID = "rollback"
	copy.BillingOperationID = "rollback"
	_, created, err := model.ReserveImageTask(&copy, &model.AsyncUsageInfo{ChannelID: 1})
	require.Error(t, err)
	require.False(t, created)
	require.Len(t, runtimeUses(t, f.store.DB), 1)
	var count int64
	require.NoError(t, f.store.DB.Model(&model.ImageTask{}).Where("id = ?", copy.ID).Count(&count).Error)
	require.Zero(t, count)
}
func TestAdmissionRuntimeImageLateWalletCannotStartExpiredAttempt(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	task, _ := reserveAdmissionImage(t, f, "late-wallet", 0)
	attempt, err := model.PrepareImageTaskAttempt(context.Background(), task, f.identities[0], 0)
	require.NoError(t, err)
	task.AdmissionAttempt = attempt
	// Preserve exact generation while making the actual frozen deadline past.
	task.CreatedAt = time.Now().Add(-model.AsyncGenerationDeadline - time.Second)
	require.NoError(t, f.store.DB.Model(&model.ImageTask{}).Where("id = ?", task.ID).UpdateColumn("created_at", task.CreatedAt).Error)
	task.Attempts = []model.ImageTaskAttempt{{ChannelID: 1, Failure: failover.Failure{Acceptance: failover.Unknown}}}
	channel, err := model.GetChannelByID(1)
	require.NoError(t, err)
	require.ErrorIs(t, model.SaveImageTaskAttempt(task, 0, 1, channel.BaseURL), model.ErrChannelAdmissionConflict)
	var persisted model.ImageTask
	require.NoError(t, f.store.DB.First(&persisted, "id = ?", task.ID).Error)
	require.Empty(t, persisted.Attempts)
	require.Len(t, runtimeUses(t, f.store.DB), 2)
}
func TestAdmissionRuntimeImageAttemptRollsBackIfClockExpiresDuringWrite(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	task, _ := reserveAdmissionImage(t, f, "clock", 0)
	task.CreatedAt = time.Now().Add(-model.AsyncGenerationDeadline + 400*time.Millisecond)
	require.NoError(t, f.store.DB.Model(&model.ImageTask{}).Where("id = ?", task.ID).UpdateColumn("created_at", task.CreatedAt).Error)
	require.NoError(t, f.store.DB.Callback().Create().After("gorm:create").Register("test:expire-image-attempt", func(tx *gorm.DB) {
		if tx.Statement.Table == "channel_admission_uses" {
			time.Sleep(600 * time.Millisecond)
		}
	}))
	defer f.store.DB.Callback().Create().Remove("test:expire-image-attempt")
	_, err := model.PrepareImageTaskAttempt(context.Background(), task, f.identities[0], 0)
	require.ErrorIs(t, err, model.ErrChannelAdmissionConflict)
	require.Len(t, runtimeUses(t, f.store.DB), 1)
}
