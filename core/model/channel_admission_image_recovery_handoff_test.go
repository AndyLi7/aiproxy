package model_test

import (
	"context"
	"encoding/json"
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

func installImageRecoveryHandoff(t *testing.T, db *gorm.DB) {
	t.Helper()
	installImageRecoveryClaim(t, db)
	raw, err := os.ReadFile("../migrations/channel-image-recovery-handoff-v1/" + db.Dialector.Name() + ".sql")
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

func imageRecoveryHandoffs(t *testing.T, db *gorm.DB) []model.ChannelAdmissionImageRecoveryHandoff {
	t.Helper()
	var rows []model.ChannelAdmissionImageRecoveryHandoff
	require.NoError(t, db.Order("offered_at_ms,id").Find(&rows).Error)
	return rows
}
func claimedImageHandoffTask(t *testing.T, f *admissionFixture) *model.ImageTask {
	t.Helper()
	task := imageRecoveryTask(t, f)
	won, err := model.ClaimImagePrepaymentRecovery(t.Context(), task, time.Now())
	require.NoError(t, err)
	require.True(t, won)
	return task
}
func TestAdmissionImageRecoveryHandoffAtomicExitAndSingleAccept(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installImageRecoveryHandoff(t, db)
	task := claimedImageHandoffTask(t, f)
	h, err := model.PrepareImageRecoveryHandoff(t.Context(), task)
	require.NoError(t, err)
	require.NotNil(t, h)
	raw, err := json.Marshal(h)
	require.NoError(t, err)
	require.Equal(t, "{}", string(raw))
	require.Error(t, model.ReleaseImagePrepaymentRecovery(t.Context(), task))
	require.Error(t, h.ReleaseAndRecordExit(t.Context(), false))
	_, err = model.PrepareImageRecoveryHandoff(t.Context(), task)
	require.Error(t, err)
	require.Len(t, imageRecoveryHandoffs(t, db), 1)
	type result struct {
		ok  bool
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() { ok, e := h.Accept(context.Background()); results <- result{ok, e} }()
	}
	n := 0
	for range 2 {
		r := <-results
		require.NoError(t, r.err)
		if r.ok {
			n++
		}
	}
	require.Equal(t, 1, n)
	before, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)
	require.NoError(t, h.ReleaseAndRecordExit(t.Context(), true))
	row := imageRecoveryHandoffs(t, db)[0]
	require.Equal(t, "exited", row.State)
	require.Equal(t, "error_returned", row.ExecutionState)
	require.Equal(t, "unproven", row.ContinuationState)
	require.Empty(t, imageClaimRows(t, db)[0].Owner)
	after, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)
	require.Equal(t, before.BillingNextCheckAt, after.BillingNextCheckAt)
	require.Error(t, h.ReleaseAndRecordExit(t.Context(), true))
	require.Equal(t, row, imageRecoveryHandoffs(t, db)[0])
	require.Equal(t, "open", runtimeUses(t, db)[0].State)
}
func TestAdmissionImageRecoveryHandoffStorage(t *testing.T) {
	for _, mode := range []string{"absent", "missing-table", "missing-protocol", "version", "column", "write", "late-no-claim", "valid"} {
		t.Run(mode, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			if mode != "late-no-claim" {
				installImageRecoveryClaim(t, db)
			}
			task := claimedImageHandoffTask(t, f)
			if mode != "absent" {
				installImageRecoveryHandoff(t, db)
			}
			switch mode {
			case "missing-table":
				require.NoError(t, db.Migrator().DropTable(&model.ChannelAdmissionImageRecoveryHandoff{}))
			case "missing-protocol":
				require.NoError(t, db.Where("id=6").Delete(&model.ChannelAdmissionExecutionProtocol{}).Error)
			case "version":
				require.NoError(t, db.Model(&model.ChannelAdmissionExecutionProtocol{}).Where("id=6").Update("version", 99).Error)
			case "column":
				require.NoError(t, db.Exec("ALTER TABLE channel_admission_image_recovery_handoffs DROP COLUMN accepted_at_ms").Error)
			case "write":
				require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:offer", func(tx *gorm.DB) {
					if tx.Statement.Table == "channel_admission_image_recovery_handoffs" {
						tx.AddError(errors.New("offer failed"))
					}
				}))
				defer db.Callback().Create().Remove("test:offer")
			}
			h, err := model.PrepareImageRecoveryHandoff(t.Context(), task)
			if mode == "absent" || mode == "late-no-claim" {
				require.NoError(t, err)
				require.Nil(t, h)
			} else if mode == "valid" {
				require.NoError(t, err)
				require.NotNil(t, h)
			} else {
				require.Error(t, err)
				require.Nil(t, h)
			}
			if mode == "write" {
				require.Empty(t, imageRecoveryHandoffs(t, db))
				require.NotEmpty(t, imageClaimRows(t, db)[0].Owner)
			}
		})
	}
}
func TestAdmissionImageRecoveryHandoffInvalidationAndRollback(t *testing.T) {
	for _, phase := range []string{"accept", "exit"} {
		for _, mode := range []string{"owner", "deadline", "generation", "contract", "storage", "proof", "facts", "journal-write", "claim-write", "outage", "epoch"} {
			t.Run(phase+"/"+mode, func(t *testing.T) {
				f := runtimeAdmissionFixture(t)
				db := f.store.DB
				installImageRecoveryHandoff(t, db)
				task := claimedImageHandoffTask(t, f)
				h, err := model.PrepareImageRecoveryHandoff(t.Context(), task)
				require.NoError(t, err)
				if phase == "exit" {
					a, e := h.Accept(t.Context())
					require.NoError(t, e)
					require.True(t, a)
				}
				switch mode {
				case "owner":
					require.NoError(t, db.Model(&model.ChannelAdmissionImageRecoveryClaim{}).Where("1=1").Update("owner", strings.Repeat("b", 64)).Error)
				case "deadline":
					require.NoError(t, db.Model(&model.ChannelAdmissionImageRecoveryClaim{}).Where("1=1").Update("until_micros", time.Now().UTC().Add(-time.Second).UnixMicro()).Error)
				case "generation":
					require.NoError(t, db.Model(&model.ImageTask{}).Where("id = ?", task.ID).Update("created_at", task.CreatedAt.Add(time.Second)).Error)
				case "contract":
					require.NoError(t, db.Model(&model.ImageTask{}).Where("id = ?", task.ID).Update("validation_contract", `{"changed":true}`).Error)
				case "storage":
					require.NoError(t, db.Model(&model.ChannelAdmissionControl{}).Where("id=1").Update("storage_generation", "restored").Error)
				case "proof":
					require.NoError(t, db.Model(&model.ChannelAdmissionProof{}).Where("id = ?", f.proof.ProofID).Update("revoked", true).Error)
				case "facts":
					require.NoError(t, db.Model(&model.ChannelAdmissionUse{}).Where("id = ?", task.AdmissionGeneration).Update("admitted_at_ms", 1).Error)
				case "outage":
					restore := model.ConfigureChannelAdmissionRuntime(f.store.Executor, nil)
					defer restore()
				case "epoch":
					restore := advanceNativeExecutionEpoch(t, f, 2, false)
					defer restore()
				case "journal-write", "claim-write":
					table := "channel_admission_image_recovery_handoffs"
					if mode == "claim-write" {
						table = "channel_admission_image_recovery_claims"
					}
					require.NoError(t, db.Callback().Update().After("gorm:update").Register("test:fail", func(tx *gorm.DB) {
						if tx.Statement.Table == table {
							tx.AddError(errors.New("write failed"))
						}
					}))
					defer db.Callback().Update().Remove("test:fail")
				}
				before := imageClaimRows(t, db)[0]
				prior := imageRecoveryHandoffs(t, db)[0]
				if phase == "accept" {
					_, err = h.Accept(t.Context())
				} else {
					err = h.ReleaseAndRecordExit(t.Context(), false)
				}
				success := mode == "outage" || mode == "epoch" || (phase == "accept" && mode == "claim-write")
				if success {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
					require.Equal(t, before, imageClaimRows(t, db)[0])
					require.Equal(t, prior, imageRecoveryHandoffs(t, db)[0])
				}
				require.Error(t, model.ReleaseImagePrepaymentRecovery(t.Context(), task), "never downgrade a journalled claim")
				require.Equal(t, "open", runtimeUses(t, db)[0].State)
			})
		}
	}
}
func TestAdmissionImageRecoveryHandoffExpiryDuringWrites(t *testing.T) {
	assertImageRecoveryHandoffExpiryDuringWrites(t, runtimeAdmissionFixture)
}
func assertImageRecoveryHandoffExpiryDuringWrites(t *testing.T, fixture func(*testing.T) *admissionFixture) {
	for _, phase := range []string{"offer", "accept", "exit", "release"} {
		t.Run(phase, func(t *testing.T) {
			f := fixture(t)
			db := f.store.DB
			installImageRecoveryHandoff(t, db)
			task := claimedImageHandoffTask(t, f)
			deadline := time.Now().Add(500 * time.Millisecond)
			require.NoError(t, model.ShortenImageRecoveryDeadlineForTest(task, deadline))
			var h *model.ImageRecoveryHandoffHandle
			var err error
			if phase != "offer" {
				h, err = model.PrepareImageRecoveryHandoff(t.Context(), task)
				require.NoError(t, err)
			}
			if phase == "exit" || phase == "release" {
				_, err = h.Accept(t.Context())
				require.NoError(t, err)
			}
			table := "channel_admission_image_recovery_handoffs"
			if phase == "release" {
				table = "channel_admission_image_recovery_claims"
			}
			pause := func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					if delay := time.Until(deadline.Add(30 * time.Millisecond)); delay > 0 {
						time.Sleep(delay)
					}
				}
			}
			if phase == "offer" {
				require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:expire", pause))
				defer db.Callback().Create().Remove("test:expire")
				_, err = model.PrepareImageRecoveryHandoff(t.Context(), task)
			} else {
				require.NoError(t, db.Callback().Update().After("gorm:update").Register("test:expire", pause))
				defer db.Callback().Update().Remove("test:expire")
				if phase == "accept" {
					_, err = h.Accept(t.Context())
				} else {
					err = h.ReleaseAndRecordExit(t.Context(), false)
				}
			}
			require.Error(t, err)
			require.NotEmpty(t, imageClaimRows(t, db)[0].Owner)
			rows := imageRecoveryHandoffs(t, db)
			if phase == "offer" {
				require.Empty(t, rows)
			} else {
				require.Equal(t, "active", rows[0].ExecutionState)
				require.Zero(t, rows[0].ExitedAtMs)
			}
		})
	}
}

func nextImageHandoffClaim(t *testing.T, f *admissionFixture, task *model.ImageTask) *model.ImageTask {
	t.Helper()
	require.NoError(t, f.store.DB.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("billing_next_check_at", time.Now().UTC().Add(-time.Second)).Error)
	fresh, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)
	won, err := model.ClaimImagePrepaymentRecovery(t.Context(), fresh, time.Now())
	require.NoError(t, err)
	require.True(t, won)
	return fresh
}
func TestAdmissionImageRecoveryHandoffProofAnchorsAndGaps(t *testing.T) {
	for _, mode := range []string{"durable-outage", "historical-expiry", "new-epoch-fresh", "new-epoch-outage", "new-member-outage", "uncovered-outage-restored", "explicit-conflict", "revoked", "corrupt-proof", "corrupt-history", "missing-use", "changed-core"} {
		t.Run(mode, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installImageRecoveryHandoff(t, db)
			var calls int
			loader := f.store.LoadDeployment
			var outage bool
			custom := func(ctx context.Context) (model.VerifiedDeploymentProof, error) {
				calls++
				if outage {
					if mode == "explicit-conflict" {
						return model.VerifiedDeploymentProof{}, model.ErrChannelAdmissionConflict
					}
					return model.VerifiedDeploymentProof{}, errors.New("source unavailable")
				}
				return loader(ctx)
			}
			restore := model.ConfigureChannelAdmissionRuntime(f.store.Executor, custom)
			defer restore()
			if mode == "historical-expiry" {
				f.proof.ValidUntilMs = time.Now().Add(700 * time.Millisecond).UnixMilli()
				f.proof.RevocationFreshUntilMs = f.proof.ValidUntilMs
				hash, e := f.proof.Fingerprint()
				require.NoError(t, e)
				require.NoError(t, db.Model(&model.ChannelAdmissionProof{}).Where("id=?", f.proof.ProofID).Update("fact_hash", hash).Error)
			}
			task := claimedImageHandoffTask(t, f)
			if mode == "uncovered-outage-restored" || mode == "explicit-conflict" {
				outage = true
			}
			h, err := model.PrepareImageRecoveryHandoff(t.Context(), task)
			if mode == "explicit-conflict" {
				require.Error(t, err)
				require.Nil(t, h)
				require.Empty(t, imageRecoveryHandoffs(t, db))
				return
			}
			require.NoError(t, err)
			if mode == "uncovered-outage-restored" {
				require.Nil(t, h)
				require.NoError(t, model.ReleaseImagePrepaymentRecovery(t.Context(), task))
				require.Empty(t, imageRecoveryHandoffs(t, db))
				outage = false
			} else {
				require.NotNil(t, h)
				ok, e := h.Accept(t.Context())
				require.NoError(t, e)
				require.True(t, ok)
				require.NoError(t, h.ReleaseAndRecordExit(t.Context(), false))
			}
			original := imageRecoveryHandoffs(t, db)
			beforeCalls := calls
			switch mode {
			case "durable-outage":
				outage = true
			case "historical-expiry":
				time.Sleep(time.Until(time.UnixMilli(f.proof.ValidUntilMs).Add(30 * time.Millisecond)))
				outage = true
			case "new-epoch-fresh", "new-epoch-outage":
				reset := advanceNativeExecutionEpoch(t, f, 2, false)
				defer reset()
				if mode == "new-epoch-outage" {
					reset2 := model.ConfigureChannelAdmissionRuntime(f.store.Executor, nil)
					defer reset2()
				}
			case "new-member-outage":
				executor := f.store.Executor
				executor.InstanceID = "new-member"
				reset := model.ConfigureChannelAdmissionRuntime(executor, nil)
				defer reset()
			case "revoked":
				require.NoError(t, db.Model(&model.ChannelAdmissionProof{}).Where("id=?", f.proof.ProofID).Update("revoked", true).Error)
			case "corrupt-proof":
				require.NoError(t, db.Model(&model.ChannelAdmissionProof{}).Where("id=?", f.proof.ProofID).Update("fact_hash", strings.Repeat("c", 64)).Error)
			case "corrupt-history":
				require.NoError(t, db.Model(&model.ChannelAdmissionImageRecoveryHandoff{}).Where("1=1").Update("use_facts_json", "[]").Error)
			case "missing-use":
				require.NoError(t, db.Where("id=?", task.AdmissionGeneration).Delete(&model.ChannelAdmissionUse{}).Error)
			case "changed-core":
				require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("expected_images", 99).Error)
			}

			next := nextImageHandoffClaim(t, f, task)
			nextHandle, e := model.PrepareImageRecoveryHandoff(t.Context(), next)
			success := mode == "durable-outage" || mode == "historical-expiry" || mode == "new-epoch-fresh" || mode == "uncovered-outage-restored"
			if !success {
				require.Error(t, e)
				require.Nil(t, nextHandle)
				require.Len(t, imageRecoveryHandoffs(t, db), len(original))
				return
			}
			require.NoError(t, e)
			require.NotNil(t, nextHandle)
			if mode == "durable-outage" || mode == "historical-expiry" {
				require.Equal(t, beforeCalls, calls, "durable member anchor never reloads unavailable/expired source")
			}
			ok, e := nextHandle.Accept(t.Context())
			require.NoError(t, e)
			require.True(t, ok)
			require.NoError(t, nextHandle.ReleaseAndRecordExit(t.Context(), false))
			rows := imageRecoveryHandoffs(t, db)
			require.Len(t, rows, len(original)+1)
			for _, old := range original {
				var current model.ChannelAdmissionImageRecoveryHandoff
				require.NoError(t, db.First(&current, "id=?", old.ID).Error)
				require.Equal(t, old, current)
			}
			for _, row := range rows {
				require.Equal(t, "unproven", row.ContinuationState)
			}
		})
	}
}

func TestAdmissionImageRecoveryHandoffLostPrepareResponse(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installImageRecoveryHandoff(t, db)
	task := claimedImageHandoffTask(t, f)
	_, err := model.PrepareImageRecoveryHandoff(t.Context(), task)
	require.NoError(t, err)
	model.ForgetImageHandoffResponseForTest(task)
	require.Error(t, model.ReleaseImagePrepaymentRecovery(t.Context(), task))
	_, err = model.PrepareImageRecoveryHandoff(t.Context(), task)
	require.Error(t, err)
	rows := imageRecoveryHandoffs(t, db)
	require.Len(t, rows, 1)
	require.Equal(t, "offered", rows[0].State)
	require.Equal(t, "active", rows[0].ExecutionState)
	require.NotEmpty(t, imageClaimRows(t, db)[0].Owner)
}
func TestAdmissionImageRecoveryHandoffHistoricalAttemptEvidence(t *testing.T) {
	for _, mode := range []string{"missing", "mutated", "foreign-resource", "foreign-request", "truncated", "forged-extra", "old-scope-route"} {
		t.Run(mode, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installImageRecoveryHandoff(t, db)
			task, _ := reserveAdmissionImage(t, f, "attempt-history", 0)
			require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("prepayment_quote_json", `{"version":1}`).Error)
			var err error
			task, err = model.GetImageTask(task.ID, task.GroupID, task.TokenID)
			require.NoError(t, err)
			require.NoError(t, model.CaptureImageTaskGeneration(task))
			attempt, err := model.PrepareImageTaskAttempt(t.Context(), task, f.identities[0], 0)
			require.NoError(t, err)
			task.AdmissionAttempt = attempt
			task.Attempts = []model.ImageTaskAttempt{{ChannelID: 1, StartedAt: time.Now(), Failure: failover.Failure{Acceptance: failover.Unknown}}}
			channel, err := model.GetChannelByID(1)
			require.NoError(t, err)
			require.NoError(t, model.SaveImageTaskAttempt(task, 0, 1, channel.BaseURL))
			task, err = model.GetImageTask(task.ID, task.GroupID, task.TokenID)
			require.NoError(t, err)
			won, err := model.ClaimImagePrepaymentRecovery(t.Context(), task, time.Now())
			require.NoError(t, err)
			require.True(t, won)
			h, err := model.PrepareImageRecoveryHandoff(t.Context(), task)
			require.NoError(t, err)
			ok, err := h.Accept(t.Context())
			require.NoError(t, err)
			require.True(t, ok)
			if mode != "old-scope-route" {
				require.NoError(t, h.ReleaseAndRecordExit(t.Context(), false))
			}
			switch mode {
			case "missing":
				require.NoError(t, db.Where("id=?", attempt).Delete(&model.ChannelAdmissionUse{}).Error)
			case "mutated":
				require.NoError(t, db.Model(&model.ChannelAdmissionUse{}).Where("id=?", attempt).Update("admitted_at_ms", 1).Error)
			case "foreign-resource":
				require.NoError(t, db.Model(&model.ChannelAdmissionUse{}).Where("id=?", attempt).Update("resource_key", strings.Repeat("b", 64)).Error)
			case "foreign-request":
				require.NoError(t, db.Model(&model.ChannelAdmissionUse{}).Where("id=?", attempt).Update("request_hash", strings.Repeat("b", 64)).Error)
			case "truncated":
				require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("attempts", "[]").Error)
			case "forged-extra":
				task.Attempts = append(task.Attempts, task.Attempts[0])
				raw, e := json.Marshal(task.Attempts)
				require.NoError(t, e)
				require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("attempts", string(raw)).Error)
			case "old-scope-route":
				require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("upstream_model", "new-route").Error)
			}
			before := imageRecoveryHandoffs(t, db)
			if mode == "old-scope-route" {
				require.Error(t, h.ReleaseAndRecordExit(t.Context(), false))
				require.NotEmpty(t, imageClaimRows(t, db)[0].Owner)
			} else {
				next := nextImageHandoffClaim(t, f, task)
				_, e := model.PrepareImageRecoveryHandoff(t.Context(), next)
				require.Error(t, e)
			}
			require.Equal(t, before, imageRecoveryHandoffs(t, db))
		})
	}
}

func TestAdmissionImageRecoveryHandoffLoadsProofOutsideTransaction(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installImageRecoveryHandoff(t, db)
	loader := f.store.LoadDeployment
	calls := 0
	restore := model.ConfigureChannelAdmissionRuntime(f.store.Executor, func(ctx context.Context) (model.VerifiedDeploymentProof, error) {
		calls++
		// Independent transaction can acquire the control row while the source loads.
		err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return tx.Exec("UPDATE channel_admission_controls SET revision=revision WHERE id=1").Error
		})
		if err != nil {
			return model.VerifiedDeploymentProof{}, err
		}
		return loader(ctx)
	})
	defer restore()
	task := claimedImageHandoffTask(t, f)
	calls = 0
	h, err := model.PrepareImageRecoveryHandoff(t.Context(), task)
	require.NoError(t, err)
	require.NotNil(t, h)
	require.Equal(t, 1, calls)
}

func TestAdmissionImageRecoveryHandoffRejectsStaleCoreAtPrepare(t *testing.T) {
	for _, field := range []string{"expected_images", "request_model", "validation_contract"} {
		t.Run(field, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installImageRecoveryHandoff(t, db)
			task := claimedImageHandoffTask(t, f)
			var value any = "changed"
			if field == "expected_images" {
				value = 42
			}
			require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update(field, value).Error)
			h, err := model.PrepareImageRecoveryHandoff(t.Context(), task)
			require.Error(t, err)
			require.Nil(t, h)
			require.Empty(t, imageRecoveryHandoffs(t, db))
			require.NotEmpty(t, imageClaimRows(t, db)[0].Owner)
		})
	}
}
