//nolint:testpackage // White-box coverage needs the package-private installTaskImageRecoveryClaim fixture or implementation.
package task

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func installTaskImageRecoveryHandoff(t *testing.T, db *gorm.DB) {
	t.Helper()
	installTaskImageRecoveryClaim(t, db)
	require.NoError(t, db.AutoMigrate(&model.ChannelAdmissionImageRecoveryHandoff{}))
	require.NoError(t, db.Save(&model.ChannelAdmissionExecutionProtocol{ID: 6, Version: 1}).Error)
}

func taskImageHandoffs(t *testing.T, db *gorm.DB) []model.ChannelAdmissionImageRecoveryHandoff {
	t.Helper()

	var rows []model.ChannelAdmissionImageRecoveryHandoff
	require.NoError(t, db.Order("offered_at_ms,id").Find(&rows).Error)

	return rows
}

func TestImageRecoveryHandoffActualLateWalletResponsesAndMultipleActions(t *testing.T) {
	for _, mode := range []string{"get", "delivered-then-get", "never-held", "refund-get", "refund-second-write-failure"} {
		t.Run(mode, func(t *testing.T) {
			db, _, identity := imageRecoveryRuntime(t)
			installTaskImageRecoveryHandoff(t, db)

			status := "queued"
			if mode == "delivered-then-get" {
				status = "completed"
			}

			if mode == "never-held" {
				status = "failed"
			}

			task := reserveTaskImageRecovery(t, db, identity, "image", status)

			var (
				calls     atomic.Int32
				gotRefund atomic.Bool
				reads     atomic.Int32
			)
			if mode == "refund-second-write-failure" {
				require.NoError(
					t,
					db.Callback().
						Query().
						Before("gorm:query").
						Register("test:between", func(tx *gorm.DB) {
							if tx.Statement.Table == "image_tasks" && gotRefund.Load() &&
								reads.Add(1) == 2 {
								_ = tx.AddError(
									errors.New("synthetic second receipt write failure"),
								)
							}
						}),
				)
				defer func() {
					if err := db.Callback().Query().Remove("test:between"); err != nil {
						t.Error(err)
					}
				}()
			}

			imageRecoveryWallet(
				t,
				func(w http.ResponseWriter, r *http.Request, c balance.PrepaymentCommand) {
					calls.Add(1)

					if mode != "refund-second-write-failure" {
						if err := db.Model(&model.ChannelAdmissionImageRecoveryClaim{}).
							Where("1=1").
							Update("owner", strings.Repeat("b", 64)).
							Error; err != nil {
							t.Error(err)
						}
					}

					switch mode {
					case "never-held":
						w.WriteHeader(http.StatusNotFound)
						_, _ = w.Write([]byte(`{"code":-1,"message":"Prepayment not found"}`))
					case "refund-get", "refund-second-write-failure":
						gotRefund.Store(true)
						refundedImageReceipt(w, c)
					default:
						pendingImageReceipt(w, c)
					}
				},
			)
			recoverImagePrepayments(t.Context())
			require.EqualValues(t, 1, calls.Load(), "lost claim cannot send the next wallet action")
			rows := taskImageHandoffs(t, db)
			require.Len(t, rows, 1)

			if mode == "refund-second-write-failure" {
				require.Equal(t, "exited", rows[0].State)
				require.Equal(t, "error_returned", rows[0].ExecutionState)
			} else {
				require.Equal(t, "accepted", rows[0].State)
				require.Equal(t, "active", rows[0].ExecutionState)
			}

			require.Equal(t, "unproven", rows[0].ContinuationState)

			fresh, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
			require.NoError(t, err)
			require.Nil(t, fresh.Billing)
			require.False(t, fresh.BillingSettled)

			if mode == "refund-second-write-failure" {
				require.Equal(t, "failed", fresh.Status)
			} else {
				require.Equal(t, status, fresh.Status)
			}

			row := taskImageClaim(t, db)
			if mode == "refund-second-write-failure" {
				require.Empty(t, row.Owner)
				require.Zero(t, row.UntilMicros)
			} else {
				require.Equal(t, strings.Repeat("b", 64), row.Owner)
				require.NotZero(t, row.UntilMicros)
			}
		})
	}
}

func TestImageRecoveryHandoffActualOwnedCleanupAndAbnormalReturn(t *testing.T) {
	for _, mode := range []string{"blocked-cleanup", "panic", "goexit", "release-write"} {
		t.Run(mode, func(t *testing.T) {
			db, _, identity := imageRecoveryRuntime(t)
			installTaskImageRecoveryHandoff(t, db)
			reserveTaskImageRecovery(t, db, identity, "image", "queued")
			imageRecoveryWallet(
				t,
				func(w http.ResponseWriter, r *http.Request, c balance.PrepaymentCommand) { pendingImageReceipt(w, c) },
			)

			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()

			var observed model.ChannelAdmissionImageRecoveryClaim

			require.NoError(
				t,
				db.Callback().
					Update().
					After("gorm:update").
					Register("test:cleanup", func(tx *gorm.DB) {
						if tx.Statement.Table == "image_tasks" {
							if values, ok := tx.Statement.Dest.(map[string]any); ok &&
								values["billing"] != nil {
								switch mode {
								case "blocked-cleanup":
									defer func() { close(entered); <-release }()

									if err := tx.Session(&gorm.Session{NewDB: true}).
										First(&observed).
										Error; err != nil {
										_ = tx.AddError(err)
									}
								case "panic":
									panic("synthetic owned cleanup panic")
								case "goexit":
									runtime.Goexit()
								}
							}
						}

						if mode == "release-write" &&
							tx.Statement.Table == "channel_admission_image_recovery_claims" {
							if values, ok := tx.Statement.Dest.(map[string]any); ok &&
								values["owner"] == "" {
								_ = tx.AddError(errors.New("release failed"))
							}
						}
					}),
			)
			defer func() {
				if err := db.Callback().Update().Remove("test:cleanup"); err != nil {
					t.Error(err)
				}
			}()

			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() { _ = recover() }()

				recoverImagePrepayments(context.Background())
			}()

			if mode == "blocked-cleanup" {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("cleanup not reached")
				}

				require.NotEmpty(t, observed.Owner)

				select {
				case <-done:
					t.Fatal("returned before owned cleanup")
				default:
				}

				close(release)
			}

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not exit")
			}

			rows := taskImageHandoffs(t, db)
			require.Len(t, rows, 1)

			if mode == "blocked-cleanup" {
				require.Equal(t, "exited", rows[0].State)
			} else {
				require.Equal(t, "accepted", rows[0].State)
				require.Equal(t, "active", rows[0].ExecutionState)
			}

			row := taskImageClaim(t, db)
			if mode == "blocked-cleanup" {
				require.Empty(t, row.Owner)
			} else {
				require.NotEmpty(t, row.Owner)
			}
		})
	}
}

func TestImageRecoveryHandoffActualReclaimAndRecreatedTask(t *testing.T) {
	for _, mode := range []string{"reclaim", "recreated"} {
		t.Run(mode, func(t *testing.T) {
			db, _, identity := imageRecoveryRuntime(t)
			installTaskImageRecoveryHandoff(t, db)

			status := "queued"
			if mode == "recreated" {
				status = "submitting"
			}

			original := reserveTaskImageRecovery(t, db, identity, "image", status)

			var (
				next    *model.ImageTask
				nextRow model.ChannelAdmissionImageRecoveryClaim
			)
			imageRecoveryWallet(
				t,
				func(w http.ResponseWriter, r *http.Request, c balance.PrepaymentCommand) {
					old := taskImageClaim(t, db)
					// Simulate a lost old lease, then use the ACTUAL claim API for the next
					// holder. A coincident scheduling timestamp cannot recreate the old token.
					require.NoError(
						t,
						db.Model(&model.ChannelAdmissionImageRecoveryClaim{}).
							Where("id = ?", old.ID).
							Updates(map[string]any{"owner": "", "until_micros": 0}).
							Error,
					)

					if mode == "recreated" {
						current, err := model.GetImageTask(
							original.ID,
							original.GroupID,
							original.TokenID,
						)
						require.NoError(t, err)
						require.NoError(t, model.CaptureImageTaskGeneration(current))
						require.NoError(t, model.ReleaseImageTaskReservationGeneration(current))
						next = reserveTaskImageRecovery(t, db, identity, "image", "submitting")
					} else {
						require.NoError(
							t,
							db.Model(&model.ImageTask{}).
								Where("id = ?", original.ID).
								Update("billing_next_check_at", time.Now().UTC().Add(-time.Second)).
								Error,
						)

						var err error

						next, err = model.GetImageTask(
							original.ID,
							original.GroupID,
							original.TokenID,
						)
						require.NoError(t, err)
					}

					won, err := model.ClaimImagePrepaymentRecovery(r.Context(), next, time.Now())
					require.NoError(t, err)
					require.True(t, won)
					nextRow = taskImageClaim(t, db)
					require.NotEqual(t, old.Owner, nextRow.Owner)

					if mode == "recreated" {
						require.NotEqual(t, old.UseID, nextRow.UseID)
					}

					pendingImageReceipt(w, c)
				},
			)
			recoverImagePrepayments(t.Context())
			require.Equal(t, nextRow, taskImageClaim(t, db))
			rows := taskImageHandoffs(t, db)
			require.Len(t, rows, 1)
			require.Equal(t, "accepted", rows[0].State)
			require.Equal(t, "active", rows[0].ExecutionState)
			require.NoError(t, model.ValidateImagePrepaymentRecovery(t.Context(), next))

			fresh, err := model.GetImageTask(original.ID, original.GroupID, original.TokenID)
			require.NoError(t, err)
			require.Nil(t, fresh.Billing)
			require.False(t, fresh.BillingSettled)
		})
	}
}

func TestImageRecoveryHandoffActualFailoversAcrossEpochs(t *testing.T) {
	db, store, first := imageRecoveryRuntime(t)
	installTaskImageRecoveryHandoff(t, db)

	identities := make([]model.ChannelIdentity, 0, 3)

	identities = append(identities, first)
	for id := 2; id <= 3; id++ {
		ch := model.Channel{
			ID:      id,
			Key:     fmt.Sprintf("synthetic-key-%d", id),
			Type:    model.ChannelTypeFal,
			Status:  model.ChannelStatusEnabled,
			BaseURL: "https://example.invalid",
		}
		require.NoError(t, db.Create(&ch).Error)
		identity, err := model.ChannelIdentityOf(&ch)
		require.NoError(t, err)

		identities = append(identities, identity)
	}

	task := reserveTaskImageRecovery(t, db, first, "failovers", "submitting")
	require.NoError(
		t,
		db.Model(&model.ImageTask{}).
			Where("id=?", task.ID).
			Update("validation_contract", `{"public":"frozen"}`).
			Error,
	)

	var calls atomic.Int32
	imageRecoveryWallet(
		t,
		func(w http.ResponseWriter, r *http.Request, c balance.PrepaymentCommand) {
			calls.Add(1)
			pendingImageReceipt(w, c)
		},
	)

	var (
		original   []model.ChannelAdmissionImageRecoveryHandoff
		generation string
	)
	for index := range 3 {
		if index > 0 {
			proof, err := store.LoadDeployment(t.Context())
			require.NoError(t, err)

			proof.ProofID = fmt.Sprintf("proof-%d", index+1)
			proof.Epoch = fmt.Sprintf("epoch-%d", index+1)
			proof.Sequence = int64(index + 1)
			executor := store.Executor
			executor.Epoch = proof.Epoch
			executor.InstanceID = fmt.Sprintf("member-%d", index+1)
			proof.Executors = []model.ChannelExecutorIdentity{executor}
			raw, err := json.Marshal(proof.Executors)
			require.NoError(t, err)

			sum := sha256.Sum256(raw)
			proof.MembershipHash = hex.EncodeToString(sum[:])
			store.Executor = executor
			store.LoadDeployment = func(context.Context) (model.VerifiedDeploymentProof, error) { return proof, nil }
			_, err = store.Activate(t.Context(), int64(index))
			require.NoError(t, err)

			restore := model.ConfigureChannelAdmissionRuntime(executor, store.LoadDeployment)
			defer restore()
		}

		fresh, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
		require.NoError(t, err)
		require.NoError(t, model.CaptureImageTaskGeneration(fresh))

		if index == 0 {
			generation = fresh.AdmissionGeneration
		}

		require.Equal(t, generation, fresh.AdmissionGeneration)
		attempt, err := model.PrepareImageTaskAttempt(t.Context(), fresh, identities[index], index)
		require.NoError(t, err)

		fresh.AdmissionAttempt = attempt
		fresh.KeyFingerprint = identities[index].KeyDigest
		fresh.UpstreamModel = fmt.Sprintf("upstream-%d", index)
		fresh.ValidationContract = fmt.Sprintf(
			`{"public":"frozen","selected_provider_binding":{"route":%d}}`,
			index,
		)
		fresh.Attempts = append(
			fresh.Attempts,
			model.ImageTaskAttempt{
				ChannelID: index + 1,
				StartedAt: time.Now(),
				Failure:   failover.Failure{Acceptance: failover.Unknown},
			},
		)
		require.NoError(
			t,
			model.SaveImageTaskAttempt(fresh, index, index+1, "https://example.invalid"),
		)
		fresh.Attempts[index].Failure.Acceptance = failover.NotAccepted
		fresh.Attempts[index].Retry = true
		require.NoError(
			t,
			model.SaveImageTaskAttempt(fresh, index+1, index+1, "https://example.invalid"),
		)
		require.NoError(
			t,
			db.Model(&model.ImageTask{}).
				Where("id=?", task.ID).
				Update("billing_next_check_at", time.Now().UTC().Add(-time.Second)).
				Error,
		)
		recoverImagePrepayments(t.Context())
		rows := taskImageHandoffs(t, db)

		require.Len(t, rows, index+1)
		require.EqualValues(t, 2*(index+1), calls.Load())

		for _, old := range original {
			var current model.ChannelAdmissionImageRecoveryHandoff
			require.NoError(t, db.First(&current, "id=?", old.ID).Error)
			require.Equal(t, old, current)
		}

		for _, row := range rows {
			require.Equal(t, generation, row.UseID)
			require.Equal(t, "exited", row.State)
			require.Equal(t, "returned", row.ExecutionState)
			require.Equal(t, "unproven", row.ContinuationState)
			require.Equal(t, rows[0].CoreHash, row.CoreHash)
		}

		original = rows

		require.Empty(t, taskImageClaim(t, db).Owner)
	}

	require.NotEqual(t, original[0].TaskHash, original[1].TaskHash)
	require.NotEqual(t, original[1].TaskHash, original[2].TaskHash)

	var uses []model.ChannelAdmissionUse
	require.NoError(t, db.Find(&uses).Error)
	require.Len(t, uses, 4)

	for _, use := range uses {
		require.Equal(t, "open", use.State)
	}
}

func TestImageRecoveryHandoffActualInstallationAndFailures(t *testing.T) {
	for _, mode := range []string{"valid", "wallet-error", "absent", "late-sync", "late-prepare", "outage", "partial", "version", "offer-failure", "accept-failure"} {
		t.Run(mode, func(t *testing.T) {
			db, store, identity := imageRecoveryRuntime(t)
			if mode == "absent" || mode == "late-sync" || mode == "late-prepare" {
				installTaskImageRecoveryClaim(t, db)
			} else {
				installTaskImageRecoveryHandoff(t, db)
			}

			task := reserveTaskImageRecovery(t, db, identity, "installation", "queued")

			if mode == "outage" {
				restore := model.ConfigureChannelAdmissionRuntime(store.Executor, nil)
				defer restore()
			}

			if mode == "partial" {
				require.NoError(
					t,
					db.Where("id=6").Delete(&model.ChannelAdmissionExecutionProtocol{}).Error,
				)
			}

			if mode == "version" {
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionExecutionProtocol{}).
						Where("id=6").
						Update("version", 99).
						Error,
				)
			}

			if mode == "offer-failure" {
				require.NoError(
					t,
					db.Callback().
						Create().
						After("gorm:create").
						Register("test:offer", func(tx *gorm.DB) {
							if tx.Statement.Table == "channel_admission_image_recovery_handoffs" {
								_ = tx.AddError(errors.New("offer failed"))
							}
						}),
				)
				defer func() {
					if err := db.Callback().Create().Remove("test:offer"); err != nil {
						t.Error(err)
					}
				}()
			}

			if mode == "accept-failure" {
				require.NoError(
					t,
					db.Callback().
						Update().
						After("gorm:update").
						Register("test:accept", func(tx *gorm.DB) {
							if tx.Statement.Table == "channel_admission_image_recovery_handoffs" {
								_ = tx.AddError(errors.New("accept failed"))
							}
						}),
				)
				defer func() {
					if err := db.Callback().Update().Remove("test:accept"); err != nil {
						t.Error(err)
					}
				}()
			}

			if mode == "late-prepare" {
				require.NoError(
					t,
					db.Callback().
						Create().
						After("gorm:create").
						Register("test:late", func(tx *gorm.DB) {
							if tx.Statement.Table == "channel_admission_image_recovery_claims" {
								require.NoError(
									t,
									tx.Session(&gorm.Session{NewDB: true}).
										AutoMigrate(&model.ChannelAdmissionImageRecoveryHandoff{}),
								)
								require.NoError(
									t,
									tx.Session(&gorm.Session{NewDB: true}).
										Create(&model.ChannelAdmissionExecutionProtocol{ID: 6, Version: 1}).
										Error,
								)
							}
						}),
				)
				defer func() {
					if err := db.Callback().Create().Remove("test:late"); err != nil {
						t.Error(err)
					}
				}()
			}

			var calls atomic.Int32
			imageRecoveryWallet(
				t,
				func(w http.ResponseWriter, r *http.Request, c balance.PrepaymentCommand) {
					calls.Add(1)

					if mode == "late-sync" {
						installTaskImageRecoveryHandoff(t, db)
					}

					if mode == "wallet-error" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}

					pendingImageReceipt(w, c)
				},
			)
			recoverImagePrepayments(t.Context())

			blocked := mode == "partial" || mode == "version" || mode == "offer-failure" ||
				mode == "accept-failure"
			if blocked {
				require.Zero(t, calls.Load())
				require.NotEmpty(t, taskImageClaim(t, db).Owner)
			} else {
				require.EqualValues(t, 1, calls.Load())
				require.Empty(t, taskImageClaim(t, db).Owner)
			}

			if mode == "absent" {
				return
			}

			rows := taskImageHandoffs(t, db)
			switch mode {
			case "valid", "wallet-error", "late-prepare":
				require.Len(t, rows, 1)
				require.Equal(t, "exited", rows[0].State)

				if mode == "wallet-error" {
					require.Equal(t, "error_returned", rows[0].ExecutionState)
				} else {
					require.Equal(t, "returned", rows[0].ExecutionState)
				}
			case "accept-failure":
				require.Len(t, rows, 1)
				require.Equal(t, "offered", rows[0].State)
			default:
				require.Empty(t, rows)
			}

			fresh, e := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
			require.NoError(t, e)
			require.False(t, fresh.BillingSettled)
		})
	}
}
