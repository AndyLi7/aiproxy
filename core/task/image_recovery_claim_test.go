//nolint:testpackage // White-box coverage needs the package-private recoverImagePrepayments fixture or implementation.
package task

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func imageRecoveryRuntime(
	t *testing.T,
) (*gorm.DB, *model.ChannelAdmissionStore, model.ChannelIdentity) {
	t.Helper()
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "recovery.db"))
	require.NoError(t, err)
	require.NoError(
		t,
		db.AutoMigrate(
			&model.ImageTask{},
			&model.AsyncUsageInfo{},
			&model.Log{},
			&model.Channel{},
			&model.ChannelAdmissionControl{},
			&model.ChannelAdmissionProof{},
			&model.ChannelAdmissionFamily{},
			&model.ChannelAdmissionBarrier{},
			&model.ChannelAdmissionUse{},
		),
	)
	require.NoError(t, db.Create(&model.ChannelAdmissionControl{ID: 1, Version: 1}).Error)
	ch := model.Channel{
		ID:      1,
		Key:     "synthetic-image-key",
		Type:    model.ChannelTypeFal,
		Status:  model.ChannelStatusEnabled,
		BaseURL: "https://example.invalid",
	}
	require.NoError(t, db.Create(&ch).Error)
	identity, err := model.ChannelIdentityOf(&ch)
	require.NoError(t, err)

	executor := model.ChannelExecutorIdentity{
		ServiceID:  "synthetic",
		InstanceID: "image-recovery",
		BuildSHA:   strings.Repeat("a", 64),
		Epoch:      "synthetic",
	}
	proof := model.VerifiedDeploymentProof{
		ProofID:                "synthetic",
		Epoch:                  "synthetic",
		Sequence:               1,
		StorageGeneration:      "synthetic",
		ExecutorFenceHash:      strings.Repeat("b", 64),
		Executors:              []model.ChannelExecutorIdentity{executor},
		IssuedAtMs:             time.Now().Add(-time.Minute).UnixMilli(),
		ValidUntilMs:           time.Now().Add(time.Minute).UnixMilli(),
		RevocationFreshUntilMs: time.Now().Add(time.Minute).UnixMilli(),
	}
	raw, err := json.Marshal(proof.Executors)
	require.NoError(t, err)

	sum := sha256.Sum256(raw)
	proof.MembershipHash = hex.EncodeToString(sum[:])
	store := &model.ChannelAdmissionStore{
		DB:             db,
		LogDB:          db,
		Executor:       executor,
		LoadDeployment: func(context.Context) (model.VerifiedDeploymentProof, error) { return proof, nil },
	}
	_, err = store.Activate(t.Context(), 0)
	require.NoError(t, err)

	oldDB, oldLog, oldBalance := model.DB, model.LogDB, balance.Default
	model.DB, model.LogDB = db, db
	restore := model.ConfigureChannelAdmissionRuntime(executor, store.LoadDeployment)
	t.Cleanup(func() {
		restore()

		model.DB, model.LogDB, balance.Default = oldDB, oldLog, oldBalance
		pool, e := db.DB()
		require.NoError(t, e)
		require.NoError(t, pool.Close())
	})

	return db, store, identity
}

func installTaskImageRecoveryClaim(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(
		t,
		db.AutoMigrate(
			&model.ChannelAdmissionExecution{},
			&model.ChannelAdmissionExecutionProtocol{},
			&model.ChannelAdmissionImageRecoveryClaim{},
		),
	)
	require.NoError(t, db.Save(&model.ChannelAdmissionExecutionProtocol{ID: 1, Version: 1}).Error)
	require.NoError(t, db.Save(&model.ChannelAdmissionExecutionProtocol{ID: 5, Version: 1}).Error)
}

func reserveTaskImageRecovery(
	t *testing.T,
	db *gorm.DB,
	identity model.ChannelIdentity,
	id, status string,
) *model.ImageTask {
	t.Helper()

	task := &model.ImageTask{
		ID:                    id,
		Model:                 "synthetic/image",
		GroupID:               "g",
		TokenID:               1,
		Fingerprint:           strings.Repeat("a", 64),
		BillingOperationID:    "billing-" + id,
		KeyFingerprint:        identity.KeyDigest,
		AdmissionIdentityHash: identity.IdentityHash,
		ChannelType:           model.ChannelTypeFal,
		UpstreamModel:         "synthetic",
		PrepaymentQuoteJSON:   `{"version":1}`,
	}
	info := &model.AsyncUsageInfo{
		RequestID: id,
		ChannelID: 1,
		BaseURL:   "https://example.invalid",
		GroupID:   "g",
		TokenID:   1,
	}
	_, created, err := model.ReserveImageTask(task, info)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(
		t,
		db.Model(&model.ImageTask{}).
			Where("id = ?", id).
			Updates(map[string]any{"status": status, "completed_at": time.Now().UTC().Add(-2 * time.Hour)}).
			Error,
	)
	saved, err := model.GetImageTask(id, "g", 1)
	require.NoError(t, err)

	return saved
}

func taskImageClaim(t *testing.T, db *gorm.DB) model.ChannelAdmissionImageRecoveryClaim {
	t.Helper()

	var rows []model.ChannelAdmissionImageRecoveryClaim
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1)

	return rows[0]
}

func imageRecoveryWallet(
	t *testing.T,
	fn func(http.ResponseWriter, *http.Request, balance.PrepaymentCommand),
) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var command balance.PrepaymentCommand
		if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
			t.Error(err)
			return
		}

		fn(w, r, command)
	}))
	t.Cleanup(server.Close)
	balance.Default = balance.NewExternalHTTP(server.URL, "synthetic-local-only")
}

func pendingImageReceipt(w http.ResponseWriter, c balance.PrepaymentCommand) {
	if err := json.NewEncoder(w).
		Encode(map[string]any{"code": 0, "data": map[string]any{"id": c.BillingOperationID, "status": "pending", "prepaidMicros": 0}}); err != nil {
		panic(err)
	} /* A broken test response must fail visibly. */
}

func refundedImageReceipt(w http.ResponseWriter, c balance.PrepaymentCommand) {
	if err := json.NewEncoder(w).
		Encode(map[string]any{"code": 0, "data": map[string]any{"id": c.BillingOperationID, "status": "refunded", "prepaidMicros": 0, "chargedMicros": 0, "refundMicros": 0}}); err != nil {
		panic(err)
	} /* A broken test response must fail visibly. */
}

func TestImageRecoveryActualCompatibilityAndRelease(t *testing.T) {
	for _, mode := range []string{"valid", "absent", "late", "partial", "version", "outage", "write-fails"} {
		t.Run(mode, func(t *testing.T) {
			db, store, identity := imageRecoveryRuntime(t)

			installed := mode != "absent" && mode != "late"
			if installed {
				installTaskImageRecoveryClaim(t, db)
			}

			task := reserveTaskImageRecovery(t, db, identity, "image", "queued")
			if mode == "partial" {
				require.NoError(
					t,
					db.Where("id=5").Delete(&model.ChannelAdmissionExecutionProtocol{}).Error,
				)
			}

			if mode == "version" {
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionExecutionProtocol{}).
						Where("id=5").
						Update("version", 99).
						Error,
				)
			}

			if mode == "outage" {
				restore := model.ConfigureChannelAdmissionRuntime(store.Executor, nil)
				defer restore()
			}

			if mode == "write-fails" {
				require.NoError(
					t,
					db.Callback().
						Create().
						After("gorm:create").
						Register("test:offer", func(tx *gorm.DB) {
							if tx.Statement.Table == "channel_admission_image_recovery_claims" {
								_ = tx.AddError(errors.New("write failed"))
							}
						}),
				)
				defer func() {
					if err := db.Callback().Create().Remove("test:offer"); err != nil {
						t.Error(err)
					}
				}()
			}

			var calls atomic.Int32
			imageRecoveryWallet(
				t,
				func(w http.ResponseWriter, r *http.Request, c balance.PrepaymentCommand) {
					calls.Add(1)

					if mode == "late" {
						installTaskImageRecoveryClaim(t, db)
					}

					pendingImageReceipt(w, c)
				},
			)
			recoverImagePrepayments(t.Context())

			fresh, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
			require.NoError(t, err)

			if mode == "partial" || mode == "version" || mode == "write-fails" {
				require.Zero(t, calls.Load())
				require.Nil(t, fresh.BillingNextCheckAt)
				return
			}

			require.EqualValues(t, 1, calls.Load())
			require.NotNil(t, fresh.Billing)
			require.NotNil(t, fresh.BillingNextCheckAt)
			require.True(t, fresh.BillingNextCheckAt.After(time.Now().UTC().Add(110*time.Second)))

			if installed {
				row := taskImageClaim(t, db)
				require.Empty(t, row.Owner)
				require.Zero(t, row.UntilMicros)
			} else if mode == "late" {
				var n int64
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionImageRecoveryClaim{}).Count(&n).Error,
				)
				require.Zero(t, n)
			}
		})
	}
}

func TestImageRecoveryActualLateWalletResponsesAndMultipleActions(t *testing.T) {
	for _, mode := range []string{"get", "delivered-then-get", "never-held", "refund-get", "refund-second-write-failure"} {
		t.Run(mode, func(t *testing.T) {
			db, _, identity := imageRecoveryRuntime(t)
			installTaskImageRecoveryClaim(t, db)

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

func TestImageRecoveryActualOwnedCleanupAndAbnormalReturn(t *testing.T) {
	for _, mode := range []string{"blocked-cleanup", "panic", "goexit", "release-write"} {
		t.Run(mode, func(t *testing.T) {
			db, _, identity := imageRecoveryRuntime(t)
			installTaskImageRecoveryClaim(t, db)
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

			row := taskImageClaim(t, db)
			if mode == "blocked-cleanup" {
				require.Empty(t, row.Owner)
			} else {
				require.NotEmpty(t, row.Owner)
			}
		})
	}
}

func TestImageRecoveryActualReclaimAndRecreatedTask(t *testing.T) {
	for _, mode := range []string{"reclaim", "recreated"} {
		t.Run(mode, func(t *testing.T) {
			db, _, identity := imageRecoveryRuntime(t)
			installTaskImageRecoveryClaim(t, db)

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
			require.NoError(t, model.ValidateImagePrepaymentRecovery(t.Context(), next))

			fresh, err := model.GetImageTask(original.ID, original.GroupID, original.TokenID)
			require.NoError(t, err)
			require.Nil(t, fresh.Billing)
			require.False(t, fresh.BillingSettled)
		})
	}
}

func TestImageRecoveryActualLaterBatchItemGetsFreshWindow(t *testing.T) {
	db, _, identity := imageRecoveryRuntime(t)
	installTaskImageRecoveryClaim(t, db)
	first := reserveTaskImageRecovery(t, db, identity, "first", "queued")
	second := reserveTaskImageRecovery(t, db, identity, "second", "queued")

	var calls atomic.Int32
	imageRecoveryWallet(
		t,
		func(w http.ResponseWriter, r *http.Request, c balance.PrepaymentCommand) {
			if calls.Add(1) == 1 {
				time.Sleep(220 * time.Millisecond)
			}

			pendingImageReceipt(w, c)
		},
	)
	recoverImagePrepayments(t.Context())

	one, err := model.GetImageTask(first.ID, first.GroupID, first.TokenID)
	require.NoError(t, err)
	two, err := model.GetImageTask(second.ID, second.GroupID, second.TokenID)
	require.NoError(t, err)
	require.EqualValues(t, 2, calls.Load())
	require.NotNil(t, one.BillingNextCheckAt)
	require.NotNil(t, two.BillingNextCheckAt)
	require.GreaterOrEqual(
		t,
		two.BillingNextCheckAt.Sub(*one.BillingNextCheckAt),
		200*time.Millisecond,
	)
}
