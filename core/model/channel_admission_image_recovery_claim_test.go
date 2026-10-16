package model_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"os"
	"strings"
	"testing"
	"time"
)

func installImageRecoveryClaim(t *testing.T, db *gorm.DB) {
	t.Helper()
	installExecutionJournal(t, db)
	raw, err := os.ReadFile("../migrations/channel-image-recovery-claim-v1/" + db.Dialector.Name() + ".sql")
	require.NoError(t, err)
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}
	parts := strings.Split(strings.TrimSpace(strings.Join(lines, "\n")), ";")
	require.Equal(t, "BEGIN", strings.TrimSpace(parts[0]))
	require.Equal(t, "COMMIT", strings.TrimSpace(parts[len(parts)-2]))
	for range 2 {
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			for _, part := range parts[1 : len(parts)-2] {
				if err := tx.Exec(strings.TrimSpace(part)).Error; err != nil {
					return err
				}
			}
			return nil
		}))
	}
}

func imageRecoveryTask(t *testing.T, f *admissionFixture) *model.ImageTask {
	t.Helper()
	task, _ := reserveAdmissionImage(t, f, "recovery", 0)
	require.NoError(t, f.store.DB.Model(&model.ImageTask{}).Where("id = ?", task.ID).Updates(map[string]any{"prepayment_quote_json": `{"version":1}`, "status": "queued"}).Error)
	task, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)
	return task
}
func imageClaimRows(t *testing.T, db *gorm.DB) []model.ChannelAdmissionImageRecoveryClaim {
	t.Helper()
	var rows []model.ChannelAdmissionImageRecoveryClaim
	require.NoError(t, db.Find(&rows).Error)
	return rows
}
func TestAdmissionImageRecoveryClaimUniqueAndReleaseKeepsSchedule(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installImageRecoveryClaim(t, db)
	task := imageRecoveryTask(t, f)
	before := time.Now()
	won, err := model.ClaimImagePrepaymentRecovery(t.Context(), task, before.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, won)
	row := imageClaimRows(t, db)[0]
	require.Len(t, row.Owner, 64)
	require.Greater(t, row.UntilMicros, before.Add(119*time.Second).UnixMicro())
	raw, err := json.Marshal(task)
	require.NoError(t, err)
	require.NotContains(t, string(raw), row.Owner)
	fresh, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)
	require.Equal(t, row.UntilMicros, fresh.BillingNextCheckAt.UnixMicro())
	next := *fresh.BillingNextCheckAt
	won, err = model.ClaimImagePrepaymentRecovery(t.Context(), fresh, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.False(t, won, "caller clock cannot steal")
	require.NoError(t, model.ValidateImagePrepaymentRecovery(t.Context(), task))
	require.NoError(t, model.ReleaseImagePrepaymentRecovery(t.Context(), task))
	after, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)
	require.Equal(t, next, *after.BillingNextCheckAt)
	require.Empty(t, imageClaimRows(t, db)[0].Owner)
	require.Error(t, model.ValidateImagePrepaymentRecovery(t.Context(), task))
	require.Error(t, model.ReleaseImagePrepaymentRecovery(t.Context(), task))
	// Same generation, early release/reclaim in the same wall-clock second.
	require.NoError(t, db.Model(&model.ImageTask{}).Where("id = ?", task.ID).Update("billing_next_check_at", time.Now().Add(-time.Second).UTC()).Error)
	newer, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)
	won, err = model.ClaimImagePrepaymentRecovery(t.Context(), newer, time.Now())
	require.NoError(t, err)
	require.True(t, won)
	require.NotEqual(t, row.Owner, imageClaimRows(t, db)[0].Owner)
	require.Error(t, model.ReleaseImagePrepaymentRecovery(t.Context(), task))
	require.NoError(t, model.ValidateImagePrepaymentRecovery(t.Context(), newer))
	require.Equal(t, "open", runtimeUses(t, db)[0].State)
}
func TestAdmissionImageRecoveryClaimStorageAndAtomicClaim(t *testing.T) {
	for _, mode := range []string{"absent", "partial-table", "partial-protocol", "version", "column", "claim-write", "schedule-write", "valid"} {
		t.Run(mode, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installExecutionJournal(t, db)
			task := imageRecoveryTask(t, f)
			if mode != "absent" {
				installImageRecoveryClaim(t, db)
			}
			switch mode {
			case "partial-table":
				require.NoError(t, db.Migrator().DropTable(&model.ChannelAdmissionImageRecoveryClaim{}))
			case "partial-protocol":
				require.NoError(t, db.Where("id=5").Delete(&model.ChannelAdmissionExecutionProtocol{}).Error)
			case "version":
				require.NoError(t, db.Model(&model.ChannelAdmissionExecutionProtocol{}).Where("id=5").Update("version", 99).Error)
			case "column":
				require.NoError(t, db.Exec("ALTER TABLE channel_admission_image_recovery_claims DROP COLUMN owner").Error)
			case "claim-write":
				require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:claim-fail", func(tx *gorm.DB) {
					if tx.Statement.Table == "channel_admission_image_recovery_claims" {
						tx.AddError(errors.New("claim write failed"))
					}
				}))
				defer db.Callback().Create().Remove("test:claim-fail")
			case "schedule-write":
				require.NoError(t, db.Callback().Update().After("gorm:update").Register("test:claim-fail", func(tx *gorm.DB) {
					if tx.Statement.Table == "image_tasks" {
						tx.AddError(errors.New("schedule write failed"))
					}
				}))
				defer db.Callback().Update().Remove("test:claim-fail")
			}
			won, err := model.ClaimImagePrepaymentRecovery(t.Context(), task, time.Now())
			if mode == "absent" || mode == "valid" {
				require.NoError(t, err)
				require.True(t, won)
			} else {
				require.Error(t, err)
				require.False(t, won)
				fresh, e := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
				require.NoError(t, e)
				require.Nil(t, fresh.BillingNextCheckAt)
			}
			if mode == "claim-write" || mode == "schedule-write" {
				require.Empty(t, imageClaimRows(t, db))
			}
		})
	}
}
func TestAdmissionImageRecoveryClaimRejectsLateOwnerAndGeneration(t *testing.T) {
	for _, mode := range []string{"owner-same-deadline", "deadline", "storage", "generation", "sidecar-use", "task-hash", "release-write", "billing-write", "outage"} {
		t.Run(mode, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installImageRecoveryClaim(t, db)
			task := imageRecoveryTask(t, f)
			won, err := model.ClaimImagePrepaymentRecovery(t.Context(), task, time.Now())
			require.NoError(t, err)
			require.True(t, won)
			before := imageClaimRows(t, db)[0]
			switch mode {
			case "owner-same-deadline":
				require.NoError(t, db.Model(&model.ChannelAdmissionImageRecoveryClaim{}).Where("id = ?", before.ID).Update("owner", strings.Repeat("a", 64)).Error)
			case "deadline":
				require.NoError(t, db.Model(&model.ChannelAdmissionImageRecoveryClaim{}).Where("id = ?", before.ID).Update("until_micros", before.UntilMicros+1).Error)
			case "storage":
				require.NoError(t, db.Model(&model.ChannelAdmissionControl{}).Where("id=1").Update("storage_generation", "restored").Error)
			case "generation":
				require.NoError(t, db.Model(&model.ImageTask{}).Where("id = ?", task.ID).Update("created_at", task.CreatedAt.Add(time.Second)).Error)
			case "sidecar-use":
				require.NoError(t, db.Model(&model.ChannelAdmissionImageRecoveryClaim{}).Where("id = ?", before.ID).Update("use_id", "another").Error)
			case "task-hash":
				require.NoError(t, db.Model(&model.ImageTask{}).Where("id = ?", task.ID).Update("prepayment_quote_json", `{"version":2}`).Error)
			case "outage":
				restore := model.ConfigureChannelAdmissionRuntime(f.store.Executor, nil)
				defer restore()
			case "release-write", "billing-write":
				table := "channel_admission_image_recovery_claims"
				if mode == "billing-write" {
					table = "image_tasks"
				}
				require.NoError(t, db.Callback().Update().After("gorm:update").Register("test:write-fail", func(tx *gorm.DB) {
					if tx.Statement.Table == table {
						tx.AddError(errors.New("write failed"))
					}
				}))
				defer db.Callback().Update().Remove("test:write-fail")
			}
			task.Billing = &model.ImageTaskBilling{Status: "refunded", Currency: "USD"}
			if mode == "outage" {
				require.NoError(t, model.ValidateImagePrepaymentRecovery(t.Context(), task))
				require.NoError(t, model.SaveImageTaskBilling(task))
				require.NoError(t, model.ReleaseImagePrepaymentRecovery(t.Context(), task))
				return
			}
			if mode != "release-write" && mode != "billing-write" {
				require.Error(t, model.ValidateImagePrepaymentRecovery(t.Context(), task))
				require.Error(t, model.SaveImageTaskBilling(task))
				require.Error(t, model.SetImageTaskResultGeneration(task, "failed", nil, nil))
			}
			if mode == "billing-write" {
				require.Error(t, model.SaveImageTaskBilling(task))
				fresh, e := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
				require.NoError(t, e)
				require.Nil(t, fresh.Billing)
				require.Equal(t, before, imageClaimRows(t, db)[0])
				return
			}
			retained := imageClaimRows(t, db)[0]
			require.Error(t, model.ReleaseImagePrepaymentRecovery(t.Context(), task))
			require.Equal(t, retained, imageClaimRows(t, db)[0])
			require.Equal(t, "open", runtimeUses(t, db)[0].State)
		})
	}
}
func TestAdmissionImageRecoveryClaimEligibilityAndStaleScan(t *testing.T) {
	for _, mode := range []string{"settled", "unpaid", "not-due", "group", "new-generation"} {
		t.Run(mode, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installImageRecoveryClaim(t, db)
			task := imageRecoveryTask(t, f)
			changes := map[string]any{}
			switch mode {
			case "settled":
				changes["billing_settled"] = true
			case "unpaid":
				changes["prepayment_quote_json"] = ""
			case "not-due":
				changes["billing_next_check_at"] = time.Now().UTC().Add(time.Hour)
			case "group":
				changes["group_id"] = "other"
			case "new-generation":
				changes["created_at"] = task.CreatedAt.Add(time.Second)
			}
			require.NoError(t, db.Model(&model.ImageTask{}).Where("id = ?", task.ID).Updates(changes).Error)
			won, _ := model.ClaimImagePrepaymentRecovery(t.Context(), task, time.Now())
			require.False(t, won)
			require.Empty(t, imageClaimRows(t, db))
		})
	}
}
func TestAdmissionImageRecoveryClaimLateInstallNeverRebinds(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	task := imageRecoveryTask(t, f)
	won, err := model.ClaimImagePrepaymentRecovery(t.Context(), task, time.Now())
	require.NoError(t, err)
	require.True(t, won)
	installImageRecoveryClaim(t, db)
	require.NoError(t, model.ValidateImagePrepaymentRecovery(t.Context(), task))
	require.NoError(t, model.ReleaseImagePrepaymentRecovery(t.Context(), task))
	require.Empty(t, imageClaimRows(t, db))
}
func TestAdmissionImageRecoveryClaimConcurrentOnlyOne(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installImageRecoveryClaim(t, db)
	task := imageRecoveryTask(t, f)
	type result struct {
		won bool
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		copy := *task
		go func() {
			won, err := model.ClaimImagePrepaymentRecovery(context.Background(), &copy, time.Now())
			results <- result{won, err}
		}()
	}
	count := 0
	for range 2 {
		r := <-results
		require.NoError(t, r.err)
		if r.won {
			count++
		}
	}
	require.Equal(t, 1, count)
}

func TestAdmissionImageRecoveryClaimExpiryDuringWrites(t *testing.T) {
	for _, phase := range []string{"billing", "result", "release"} {
		t.Run(phase, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installImageRecoveryClaim(t, db)
			task := imageRecoveryTask(t, f)
			won, err := model.ClaimImagePrepaymentRecovery(t.Context(), task, time.Now())
			require.NoError(t, err)
			require.True(t, won)
			deadline := time.Now().Add(400 * time.Millisecond)
			require.NoError(t, model.ShortenImageRecoveryDeadlineForTest(task, deadline))
			before := imageClaimRows(t, db)[0]
			table := "image_tasks"
			if phase == "release" {
				table = "channel_admission_image_recovery_claims"
			}
			require.NoError(t, db.Callback().Update().After("gorm:update").Register("test:expire", func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					if delay := time.Until(deadline.Add(30 * time.Millisecond)); delay > 0 {
						time.Sleep(delay)
					}
				}
			}))
			defer db.Callback().Update().Remove("test:expire")
			switch phase {
			case "billing":
				task.Billing = &model.ImageTaskBilling{Status: "refunded"}
				err = model.SaveImageTaskBilling(task)
			case "result":
				err = model.SetImageTaskResultGeneration(task, "failed", nil, nil)
			case "release":
				err = model.ReleaseImagePrepaymentRecovery(t.Context(), task)
			}
			require.Error(t, err)
			require.Equal(t, before, imageClaimRows(t, db)[0])
			fresh, e := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
			require.NoError(t, e)
			require.Equal(t, "queued", fresh.Status)
			require.Nil(t, fresh.Billing)
			require.Equal(t, before.UntilMicros, fresh.BillingNextCheckAt.UnixMicro())
		})
	}
}
