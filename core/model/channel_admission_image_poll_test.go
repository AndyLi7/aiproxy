package model_test

import (
	"context"
	"errors"
	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"os"
	"strings"
	"testing"
	"time"
)

func installImagePoll(t *testing.T, db *gorm.DB) {
	t.Helper()
	installImageRecoveryHandoff(t, db)
	raw, e := os.ReadFile("../migrations/channel-image-poll-handoff-v1/" + db.Dialector.Name() + ".sql")
	require.NoError(t, e)
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}
	parts := strings.Split(strings.TrimSpace(strings.Join(lines, "\n")), ";")
	for range 2 {
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			for _, p := range parts[1 : len(parts)-2] {
				if e := tx.Exec(strings.TrimSpace(p)).Error; e != nil {
					return e
				}
			}
			return nil
		}))
	}
}
func pollRows(t *testing.T, db *gorm.DB) []model.ChannelAdmissionImagePollHandoff {
	t.Helper()
	var rows []model.ChannelAdmissionImagePollHandoff
	require.NoError(t, db.Order("offered_at_ms,id").Find(&rows).Error)
	return rows
}
func pollFixtureOn(t *testing.T, f *admissionFixture, duration time.Duration, measuredMode ...bool) (*model.ImageTask, *model.AsyncUsageInfo) {
	t.Helper()
	installImagePoll(t, f.store.DB)
	task, _ := reserveAdmissionImage(t, f, "poll", 0)
	// Persistent prepaid binding exercises generic receipt path without synthetic
	// ordinary price/log setup; no wallet is invoked in these model tests.
	quote := `{"version":1}`
	if len(measuredMode) > 0 {
		quote = ""
	}
	require.NoError(t, f.store.DB.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("prepayment_quote_json", quote).Error)
	if len(measuredMode) > 0 {
		require.NoError(t, f.store.DB.Model(&model.AsyncUsageInfo{}).Where("id=?", task.UsageID).Update("measured_image", measuredMode[0]).Error)
	}
	require.NoError(t, f.store.DB.Model(&model.AsyncUsageInfo{}).Where("id=?", task.UsageID).Updates(map[string]any{"billing_operation_id": task.BillingOperationID, "status": model.AsyncUsageStatusPending, "next_poll_at": time.Now().Add(-time.Second)}).Error)
	task, e := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
	require.NoError(t, e)
	var u model.AsyncUsageInfo
	require.NoError(t, f.store.DB.First(&u, task.UsageID).Error)
	won, e := model.TryClaimAsyncUsageInfo(&u, "poll-original", time.Now().Add(duration), time.Now())
	require.NoError(t, e)
	require.True(t, won)
	if len(measuredMode) > 0 {
		require.NoError(t, f.store.DB.Model(&model.Log{}).Where("id=?", u.LogID).Updates(map[string]any{"billing_operation_id": u.BillingOperationID, "async_usage_status": model.AsyncUsageStatusPending}).Error)
	}
	require.Len(t, pollRows(t, f.store.DB), 1)
	return task, &u
}
func TestAdmissionImagePollLifecycle(t *testing.T) {
	for _, mode := range []string{"return-owned", "touch", "failure", "renew", "release-new-owner", "release-corrupt", "expired", "new-owner", "offer-only"} {
		t.Run(mode, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			_, u := pollFixtureOn(t, f, time.Minute)
			if mode == "offer-only" {
				require.Error(t, model.RecordImagePollReturn(t.Context(), u))
				require.Equal(t, "offered", pollRows(t, db)[0].State)
				return
			}
			ok, e := model.AcceptImagePoll(t.Context(), u)
			require.NoError(t, e)
			require.True(t, ok)
			ok, e = model.AcceptImagePoll(t.Context(), u)
			require.NoError(t, e)
			require.False(t, ok)
			switch mode {
			case "touch", "release-new-owner", "release-corrupt":
				require.NoError(t, touchPoll(t, u, time.Now().Add(-time.Second)))
			case "failure":
				ok, e = model.FailClaimedAsyncUsageInfo(u)
				require.NoError(t, e)
				require.True(t, ok)
			case "renew":
				ok, e = model.RenewImagePollClaim(t.Context(), u, time.Now().Add(2*time.Minute))
				require.NoError(t, e)
				require.True(t, ok)
			case "expired":
				require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", u.ID).Update("next_poll_at", time.Now().Add(-time.Second)).Error)
			case "new-owner":
				require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", u.ID).Update("processing_token", "new-owner").Error)
			}
			if mode == "release-new-owner" {
				var newer model.AsyncUsageInfo
				require.NoError(t, db.First(&newer, u.ID).Error)
				won, e := model.TryClaimAsyncUsageInfo(&newer, "new-owner", time.Now().Add(2*time.Minute), time.Now())
				require.NoError(t, e)
				require.True(t, won)
				ok, e := model.AcceptImagePoll(t.Context(), &newer)
				require.NoError(t, e)
				require.True(t, ok)
			}
			if mode == "release-corrupt" {
				require.NoError(t, db.Model(&model.ChannelAdmissionImagePollHandoff{}).Where("id=?", pollRows(t, db)[0].ID).Update("release_json", `{}`).Error)
			}
			e = model.RecordImagePollReturn(t.Context(), u)
			if mode == "expired" || mode == "new-owner" || mode == "release-corrupt" {
				require.Error(t, e)
				require.Equal(t, "active", pollRows(t, db)[0].ExecutionState)
			} else {
				require.NoError(t, e)
				var n int
				for _, r := range pollRows(t, db) {
					if r.State == "exited" {
						n++
						require.Equal(t, "unproven", r.ContinuationState)
					}
				}
				require.Equal(t, 1, n)
				require.Error(t, model.RecordImagePollReturn(t.Context(), u))
			}
			require.Equal(t, "open", runtimeUses(t, db)[0].State)
		})
	}
}
func TestAdmissionImagePollStorageAtomicAndLate(t *testing.T) {
	for _, mode := range []string{"late", "marker", "table", "version", "column", "offer-failure", "release-failure"} {
		t.Run(mode, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			_, u := pollFixtureOn(t, f, time.Minute)
			if mode == "release-failure" {
				ok, e := model.AcceptImagePoll(t.Context(), u)
				require.NoError(t, e)
				require.True(t, ok)
				require.NoError(t, db.Callback().Update().After("gorm:update").Register("poll:failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "channel_admission_image_poll_handoffs" {
						tx.AddError(errors.New("release failure"))
					}
				}))
				defer db.Callback().Update().Remove("poll:failure")
				require.Error(t, touchPoll(t, u, time.Now()))
				var stored model.AsyncUsageInfo
				require.NoError(t, db.First(&stored, u.ID).Error)
				require.Equal(t, "poll-original", stored.ProcessingToken)
				require.Empty(t, pollRows(t, db)[0].ReleaseJSON)
				return
			}
			// A DB-loaded copy has no private capability; installation cannot backfill it.
			if mode == "late" {
				var copy model.AsyncUsageInfo
				require.NoError(t, db.First(&copy, u.ID).Error)
				ok, e := model.AcceptImagePoll(t.Context(), &copy)
				require.NoError(t, e)
				require.True(t, ok)
				require.NoError(t, model.RecordImagePollReturn(t.Context(), &copy))
				require.Equal(t, "offered", pollRows(t, db)[0].State)
				return
			}
			switch mode {
			case "marker":
				require.NoError(t, db.Where("id=7").Delete(&model.ChannelAdmissionExecutionProtocol{}).Error)
			case "table":
				require.NoError(t, db.Migrator().DropTable(&model.ChannelAdmissionImagePollHandoff{}))
			case "version":
				require.NoError(t, db.Model(&model.ChannelAdmissionExecutionProtocol{}).Where("id=7").Update("version", 2).Error)
			case "column":
				require.NoError(t, db.Exec("ALTER TABLE channel_admission_image_poll_handoffs DROP COLUMN release_hash").Error)
			case "offer-failure":
				require.NoError(t, db.Callback().Create().After("gorm:create").Register("poll:offer", func(tx *gorm.DB) {
					if tx.Statement.Table == "channel_admission_image_poll_handoffs" {
						tx.AddError(errors.New("offer failure"))
					}
				}))
				defer db.Callback().Create().Remove("poll:offer")
			}
			require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", u.ID).Update("next_poll_at", time.Now().Add(-time.Second)).Error)
			var next model.AsyncUsageInfo
			require.NoError(t, db.First(&next, u.ID).Error)
			won, e := model.TryClaimAsyncUsageInfo(&next, "new-owner", time.Now().Add(time.Minute), time.Now())
			require.Error(t, e)
			require.False(t, won)
			var stored model.AsyncUsageInfo
			require.NoError(t, db.First(&stored, u.ID).Error)
			require.Equal(t, "poll-original", stored.ProcessingToken)
		})
	}
}
func TestAdmissionImagePollOldActiveNewClaimAndRenewal(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	_, u := pollFixtureOn(t, f, 180*time.Millisecond)
	ok, e := model.AcceptImagePoll(t.Context(), u)
	require.NoError(t, e)
	require.True(t, ok)
	initial := u.NextPollAt
	ok, e = model.RenewImagePollClaim(t.Context(), u, time.Now().Add(time.Second))
	require.NoError(t, e)
	require.True(t, ok)
	time.Sleep(time.Until(initial.Add(15 * time.Millisecond)))
	require.NoError(t, model.RecordImagePollReturn(t.Context(), u))
	// A different expired invocation remains active when a new claim is accepted.
	require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", u.ID).Update("next_poll_at", time.Now().Add(-time.Second)).Error)
	var next model.AsyncUsageInfo
	require.NoError(t, db.First(&next, u.ID).Error)
	won, e := model.TryClaimAsyncUsageInfo(&next, "second", time.Now().Add(150*time.Millisecond), time.Now())
	require.NoError(t, e)
	require.True(t, won)
	ok, e = model.AcceptImagePoll(t.Context(), &next)
	require.NoError(t, e)
	require.True(t, ok)
	time.Sleep(time.Until(next.NextPollAt.Add(10 * time.Millisecond)))
	ok, e = model.RenewImagePollClaim(t.Context(), &next, time.Now().Add(time.Minute))
	require.NoError(t, e)
	require.False(t, ok)
	before := pollRows(t, db)
	var last model.AsyncUsageInfo
	require.NoError(t, db.First(&last, u.ID).Error)
	won, e = model.TryClaimAsyncUsageInfo(&last, "third", time.Now().Add(time.Minute), time.Now())
	require.NoError(t, e)
	require.True(t, won)
	ok, e = model.AcceptImagePoll(context.Background(), &last)
	require.NoError(t, e)
	require.True(t, ok)
	require.Error(t, model.RecordImagePollReturn(t.Context(), &next))
	after := pollRows(t, db)
	require.Equal(t, before, after[:len(before)])
}

func touchPoll(t *testing.T, u *model.AsyncUsageInfo, at time.Time) error {
	t.Helper()
	u.NextPollAt = at
	return model.TouchClaimedAsyncUsageInfo(u)
}

func TestAdmissionImagePollHistoryAndProof(t *testing.T) {
	for _, mode := range []string{"durable-outage", "new-epoch", "epoch-outage", "revoked", "history-corrupt", "missing-root", "canceled-context"} {
		t.Run(mode, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			_, u := pollFixtureOn(t, f, time.Minute)
			ok, e := model.AcceptImagePoll(t.Context(), u)
			require.NoError(t, e)
			require.True(t, ok)
			require.NoError(t, touchPoll(t, u, time.Now().Add(-time.Second)))
			require.NoError(t, model.RecordImagePollReturn(t.Context(), u))
			before := pollRows(t, db)
			ctx := t.Context()
			switch mode {
			case "durable-outage":
				restore := model.ConfigureChannelAdmissionRuntime(f.store.Executor, nil)
				defer restore()
			case "new-epoch", "epoch-outage":
				restore := advanceNativeExecutionEpoch(t, f, 2, false)
				defer restore()
				if mode == "epoch-outage" {
					reset := model.ConfigureChannelAdmissionRuntime(f.store.Executor, nil)
					defer reset()
				}
			case "revoked":
				require.NoError(t, db.Model(&model.ChannelAdmissionProof{}).Where("id=?", f.proof.ProofID).Update("revoked", true).Error)
			case "history-corrupt":
				require.NoError(t, db.Model(&model.ChannelAdmissionImagePollHandoff{}).Where("id=?", before[0].ID).Update("use_facts_json", "[]").Error)
			case "missing-root":
				require.NoError(t, db.Where("id=?", before[0].UseID).Delete(&model.ChannelAdmissionUse{}).Error)
			case "canceled-context":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var next model.AsyncUsageInfo
			require.NoError(t, db.First(&next, u.ID).Error)
			won, e := model.TryClaimAsyncUsageInfoContext(ctx, &next, "new-generation", time.Now().Add(time.Minute), time.Now())
			if mode == "durable-outage" || mode == "new-epoch" {
				require.NoError(t, e)
				require.True(t, won)
				ok, e = model.AcceptImagePoll(t.Context(), &next)
				require.NoError(t, e)
				require.True(t, ok)
				require.NoError(t, model.RecordImagePollReturn(t.Context(), &next))
				require.Equal(t, before[0], pollRows(t, db)[0])
			} else {
				require.Error(t, e)
				require.False(t, won)
				require.Len(t, pollRows(t, db), 1)
			}
		})
	}
}

func TestAdmissionImagePollHistoricalAttempts(t *testing.T) {
	for _, mode := range []string{"failover", "missing-attempt", "corrupt-attempt"} {
		t.Run(mode, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installImagePoll(t, db)
			task, _ := reserveAdmissionImage(t, f, "history", 0)
			first, e := model.PrepareImageTaskAttempt(t.Context(), task, f.identities[0], 0)
			require.NoError(t, e)
			task.AdmissionAttempt = first
			task.Attempts = []model.ImageTaskAttempt{{ChannelID: 1, StartedAt: time.Now().UTC(), Failure: failover.Failure{Acceptance: failover.Unknown}}}
			channel, e := model.GetChannelByID(1)
			require.NoError(t, e)
			require.NoError(t, model.SaveImageTaskAttempt(task, 0, 1, channel.BaseURL))
			task.Attempts[0].Failure.Acceptance = failover.NotAccepted
			task.Attempts[0].Retry = true
			require.NoError(t, model.SaveImageTaskAttempt(task, 1, 1, channel.BaseURL))
			second, e := model.PrepareImageTaskAttempt(t.Context(), task, f.identities[1], 1)
			require.NoError(t, e)
			task.AdmissionAttempt = second
			task.Attempts = append(task.Attempts, model.ImageTaskAttempt{ChannelID: 2, StartedAt: time.Now().UTC(), Failure: failover.Failure{Acceptance: failover.Unknown}})
			channel, e = model.GetChannelByID(2)
			require.NoError(t, e)
			task.KeyFingerprint = f.identities[1].KeyDigest
			require.NoError(t, model.SaveImageTaskAttempt(task, 1, 2, channel.BaseURL))
			require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", task.UsageID).Updates(map[string]any{"status": model.AsyncUsageStatusPending, "billing_operation_id": task.BillingOperationID, "next_poll_at": time.Now().Add(-time.Second)}).Error)
			if mode == "missing-attempt" {
				require.NoError(t, db.Where("id=?", first).Delete(&model.ChannelAdmissionUse{}).Error)
			}
			if mode == "corrupt-attempt" {
				require.NoError(t, db.Model(&model.ChannelAdmissionUse{}).Where("id=?", first).Update("request_hash", strings.Repeat("b", 64)).Error)
			}
			var u model.AsyncUsageInfo
			require.NoError(t, db.First(&u, task.UsageID).Error)
			won, e := model.TryClaimAsyncUsageInfo(&u, "history-owner", time.Now().Add(time.Minute), time.Now())
			if mode != "failover" {
				require.Error(t, e)
				require.False(t, won)
				require.Empty(t, pollRows(t, db))
				return
			}
			require.NoError(t, e)
			require.True(t, won)
			ok, e := model.AcceptImagePoll(t.Context(), &u)
			require.NoError(t, e)
			require.True(t, ok)
			require.NoError(t, model.RecordImagePollReturn(t.Context(), &u))
			require.Equal(t, "unproven", pollRows(t, db)[0].ContinuationState)
			require.Len(t, runtimeUses(t, db), 3)
		})
	}
}
func TestAdmissionImagePollFinalWriteDeadline(t *testing.T) {
	for _, phase := range []string{"offer", "accept", "release", "return"} {
		t.Run(phase, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			_, u := pollFixtureOn(t, f, 220*time.Millisecond)
			deadline := u.NextPollAt
			if phase == "release" || phase == "return" {
				ok, e := model.AcceptImagePoll(t.Context(), u)
				require.NoError(t, e)
				require.True(t, ok)
			}
			if phase == "offer" {
				require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", u.ID).Update("next_poll_at", time.Now().Add(-time.Second)).Error)
			}
			before := pollRows(t, db)
			pause := func(tx *gorm.DB) {
				if tx.Statement.Table == "channel_admission_image_poll_handoffs" {
					if delay := time.Until(deadline.Add(15 * time.Millisecond)); delay > 0 {
						time.Sleep(delay)
					}
				}
			}
			var e error
			if phase == "offer" {
				require.NoError(t, db.Callback().Create().After("gorm:create").Register("poll:expiry", pause))
				defer db.Callback().Create().Remove("poll:expiry")
				_, e = model.TryClaimAsyncUsageInfo(u, "next-offer", deadline, time.Now())
			} else {
				require.NoError(t, db.Callback().Update().After("gorm:update").Register("poll:expiry", pause))
				defer db.Callback().Update().Remove("poll:expiry")
				switch phase {
				case "accept":
					_, e = model.AcceptImagePoll(t.Context(), u)
				case "release":
					e = touchPoll(t, u, time.Now().Add(time.Hour))
				default:
					e = model.RecordImagePollReturn(t.Context(), u)
				}
			}
			require.Error(t, e)
			require.Equal(t, before, pollRows(t, db))
			var stored model.AsyncUsageInfo
			require.NoError(t, db.First(&stored, u.ID).Error)
			require.Equal(t, "poll-original", stored.ProcessingToken)
		})
	}
}
