package task

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/labring/aiproxy/core/common/actionguard"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/ownedimage"
	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func installTaskImagePoll(t *testing.T, db *gorm.DB) {
	t.Helper()
	installTaskImageRecoveryHandoff(t, db)
	require.NoError(t, db.AutoMigrate(&model.ChannelAdmissionImagePollHandoff{}))
	require.NoError(t, db.Save(&model.ChannelAdmissionExecutionProtocol{ID: 7, Version: 1}).Error)
}
func taskPollRows(t *testing.T, db *gorm.DB) []model.ChannelAdmissionImagePollHandoff {
	t.Helper()
	var rows []model.ChannelAdmissionImagePollHandoff
	require.NoError(t, db.Order("offered_at_ms,id").Find(&rows).Error)
	return rows
}
func claimPollFixture(t *testing.T, db *gorm.DB, u *model.AsyncUsageInfo, token string, lease time.Duration) {
	t.Helper()
	require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", u.ID).Update("next_poll_at", time.Now().Add(-time.Second)).Error)
	require.NoError(t, db.First(u, u.ID).Error)
	won, e := model.TryClaimAsyncUsageInfo(u, token, time.Now().Add(lease), time.Now())
	require.NoError(t, e)
	require.True(t, won)
}
func TestImagePollExecutorActualCompletedAndFailed(t *testing.T) {
	for _, status := range []string{"completed", "failed"} {
		for _, mode := range []string{"normal", "expired-entry", "replaced-entry", "settled", "settle-then-lost", "absent", "late"} {
			t.Run(status+"/"+mode, func(t *testing.T) {
				db, task, info := immediatePrepaidFixture(t, status)
				if mode != "absent" && mode != "late" {
					installTaskImagePoll(t, db)
				}
				lease := time.Minute
				if mode == "settle-then-lost" && status == "failed" {
					lease = 300 * time.Millisecond
				}
				claimPollFixture(t, db, info, "actual-poll", lease)
				if mode == "late" {
					installTaskImagePoll(t, db)
				}
				var calls atomic.Int32
				imageRecoveryWallet(t, func(w http.ResponseWriter, r *http.Request, c balance.PrepaymentCommand) {
					calls.Add(1)
					if mode == "settle-then-lost" && status == "failed" {
						if delay := time.Until(info.NextPollAt.Add(10 * time.Millisecond)); delay > 0 {
							time.Sleep(delay)
						}
					}
					if mode == "settle-then-lost" {
						require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("next_poll_at", time.Now().Add(-time.Second)).Error)
					}
					state, charged, refund := "settled", 300000, 0
					if status == "failed" {
						state, charged, refund = "refunded", 0, 300000
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"id": c.BillingOperationID, "status": state, "prepaidMicros": 300000, "chargedMicros": charged, "refundMicros": refund}})
				})
				if mode == "settled" && status == "completed" {
					require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Updates(map[string]any{"billing_settled": true, "billing": `{"currency":"USD","status":"settled","prepaidMicros":300000,"actualMicros":400000,"refundMicros":0}`}).Error)
				}
				if mode == "expired-entry" {
					require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("next_poll_at", time.Now().Add(-time.Second)).Error)
				}
				if mode == "replaced-entry" {
					require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("processing_token", "replacement").Error)
				}
				processOneAsyncUsage(t.Context(), info)
				if mode == "absent" {
					return
				}
				rows := taskPollRows(t, db)
				if mode == "late" {
					require.Empty(t, rows)
					return
				}
				require.Len(t, rows, 1)
				if mode == "expired-entry" || mode == "replaced-entry" {
					require.Zero(t, calls.Load())
					require.Equal(t, "offered", rows[0].State)
					return
				}
				if mode == "settle-then-lost" {
					require.Equal(t, "accepted", rows[0].State)
					if status == "completed" {
						var fresh model.AsyncUsageInfo
						require.NoError(t, db.First(&fresh, info.ID).Error)
						claimPollFixture(t, db, &fresh, "next-poll", time.Minute)
						before := calls.Load()
						processOneAsyncUsage(t.Context(), &fresh)
						require.Equal(t, before, calls.Load())
						require.NoError(t, db.First(&fresh, info.ID).Error)
						require.Equal(t, model.AsyncUsageStatusCompleted, fresh.Status)
						require.Equal(t, rows[0], taskPollRows(t, db)[0])
					}
					return
				}
				require.Equal(t, "exited", rows[0].State)
				require.NotEmpty(t, rows[0].ReleaseHash)
				require.Equal(t, "unproven", rows[0].ContinuationState)
				if mode == "settled" && status == "completed" {
					require.Zero(t, calls.Load())
				} else {
					require.EqualValues(t, 2, calls.Load())
				}
				if status == "failed" {
					require.Equal(t, "error_returned", rows[0].ExecutionState)
				}
			})
		}
	}
}
func TestImagePollExecutorActualAbnormalExit(t *testing.T) {
	for _, mode := range []string{"panic", "goexit"} {
		t.Run(mode, func(t *testing.T) {
			db, _, info := immediatePrepaidFixture(t, "completed")
			installTaskImagePoll(t, db)
			claimPollFixture(t, db, info, "abnormal", time.Minute)
			var accepted atomic.Bool
			require.NoError(t, db.Callback().Update().After("gorm:update").Register("poll:accept", func(tx *gorm.DB) {
				if tx.Statement.Table == "channel_admission_image_poll_handoffs" {
					accepted.Store(true)
				}
			}))
			defer db.Callback().Update().Remove("poll:accept")
			require.NoError(t, db.Callback().Query().Before("gorm:query").Register("poll:abnormal", func(tx *gorm.DB) {
				if tx.Statement.Table == "image_tasks" && accepted.Swap(false) {
					if mode == "panic" {
						panic("synthetic body panic")
					}
					runtime.Goexit()
				}
			}))
			defer db.Callback().Query().Remove("poll:abnormal")
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() { _ = recover() }()
				processOneAsyncUsage(context.Background(), info)
			}()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("worker did not exit")
			}
			require.Equal(t, "accepted", taskPollRows(t, db)[0].State)
			require.Equal(t, "active", taskPollRows(t, db)[0].ExecutionState)
		})
	}
}
func TestImagePollExecutorMeasuredPriorityAndReceiptRollback(t *testing.T) {
	for _, mode := range []string{"normal", "journal-fails"} {
		t.Run(mode, func(t *testing.T) {
			db, task, info := immediatePrepaidFixture(t, "completed")
			require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("prepayment_quote_json", "").Error)
			require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("measured_image", true).Error)
			installTaskImagePoll(t, db)
			claimPollFixture(t, db, info, "measured", time.Minute)
			if mode == "journal-fails" {
				var n atomic.Int32
				require.NoError(t, db.Callback().Update().After("gorm:update").Register("poll:release", func(tx *gorm.DB) {
					if tx.Statement.Table == "channel_admission_image_poll_handoffs" && n.Add(1) == 2 {
						tx.AddError(errors.New("release failed"))
					}
				}))
				defer db.Callback().Update().Remove("poll:release")
			}
			processOneAsyncUsage(t.Context(), info)
			var fresh model.AsyncUsageInfo
			require.NoError(t, db.First(&fresh, info.ID).Error)
			if mode == "normal" {
				require.Equal(t, model.AsyncUsageStatusMeasurementPending, fresh.Status)
				require.Empty(t, fresh.ProcessingToken)
				require.NotEmpty(t, taskPollRows(t, db)[0].ReleaseHash)
			} else {
				require.Equal(t, model.AsyncUsageStatusPending, fresh.Status)
				require.Equal(t, "measured", fresh.ProcessingToken)
				require.Empty(t, taskPollRows(t, db)[0].ReleaseHash)
			}
		})
	}
}
func TestImagePollExecutorRenewalStopJoinsBeforeReturn(t *testing.T) {
	db, _, info := immediatePrepaidFixture(t, "completed")
	installTaskImagePoll(t, db)
	claimPollFixture(t, db, info, "join", time.Minute)
	ok, e := model.AcceptImagePoll(t.Context(), info)
	require.NoError(t, e)
	require.True(t, ok)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	require.NoError(t, db.Callback().Update().After("gorm:update").Register("poll:join", func(tx *gorm.DB) {
		if tx.Statement.Table == "async_usage_infos" {
			close(entered)
			<-release
		}
	}))
	defer db.Callback().Update().Remove("poll:join")
	ctx, stop := startAsyncUsageClaimRenewalAtInterval(context.Background(), info, time.Millisecond)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("renewal not entered")
	}
	exited := make(chan error, 1)
	go func() { stop(); exited <- model.RecordImagePollReturn(context.Background(), info) }()
	<-ctx.Done()
	select {
	case <-exited:
		t.Fatal("returned before renewal cleanup")
	case <-time.After(30 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	require.NoError(t, <-exited)
	require.Equal(t, "exited", taskPollRows(t, db)[0].State)
}

func TestImagePollExecutorTimeoutCancelActualHTTP(t *testing.T) {
	for _, mode := range []string{"valid-fallback", "expired-fallback", "expired-before-cancel", "redirect307", "redirect308"} {
		t.Run(mode, func(t *testing.T) {
			db, task, info := immediatePrepaidFixture(t, "in_progress")
			raw, e := os.ReadFile("../common/registryvalidation/testdata/seedream-4.5-v2-text-to-image.json")
			require.NoError(t, e)
			endpoint := "wan/v2.6/text-to-image"
			raw = []byte(strings.ReplaceAll(string(raw), "fal-ai/bytedance/seedream/v4.5/text-to-image", endpoint))
			var contract struct {
				Providers map[string]struct {
					Upstream registryvalidation.ProviderSpec `json:"upstream"`
				} `json:"providers"`
			}
			require.NoError(t, json.Unmarshal(raw, &contract))
			spec := contract.Providers["fal"].Upstream
			frozen, e := registryvalidation.FreezeProviderBinding(raw, registryvalidation.ProviderBinding{Provider: "fal", ID: spec.ID, Revision: spec.Revision, ContractHash: spec.ContractHash})
			require.NoError(t, e)
			var statusCalls, cancelCalls, redirectCalls atomic.Int32
			var deadline time.Time
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirect-target" {
					redirectCalls.Add(1)
					w.WriteHeader(202)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/status") {
					statusCalls.Add(1)
					_, _ = w.Write([]byte(`{"status":"IN_PROGRESS"}`))
					return
				}
				if strings.HasSuffix(r.URL.Path, "/cancel") {
					n := cancelCalls.Add(1)
					if mode == "redirect307" || mode == "redirect308" {
						w.Header().Set("Location", "/redirect-target")
						code := 307
						if mode == "redirect308" {
							code = 308
						}
						w.WriteHeader(code)
						return
					}
					if n == 1 {
						if mode == "expired-fallback" {
							time.Sleep(time.Until(deadline.Add(15 * time.Millisecond)))
						}
						w.WriteHeader(405)
						return
					}
					w.WriteHeader(202)
					_, _ = w.Write([]byte(`{}`))
					return
				}
				t.Errorf("unexpected local path %s", r.URL.Path)
				w.WriteHeader(500)
			}))
			defer server.Close()
			require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Updates(map[string]any{"validation_contract": string(frozen), "upstream_model": endpoint, "upstream_id": "abc", "queued_at": time.Now().UTC().Add(-model.AsyncGenerationDeadline - time.Minute)}).Error)
			require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Updates(map[string]any{"base_url": server.URL, "upstream_id": "abc"}).Error)
			installTaskImagePoll(t, db)
			lease := time.Minute
			if strings.HasPrefix(mode, "expired") {
				// Leave room for instrumented setup; the handler waits for the actual
				// captured deadline, so expiry is still deterministic.
				lease = 2 * time.Second
			}
			claimPollFixture(t, db, info, "timeout", lease)
			deadline = info.NextPollAt
			var walletCalls atomic.Int32
			imageRecoveryWallet(t, func(w http.ResponseWriter, r *http.Request, c balance.PrepaymentCommand) {
				walletCalls.Add(1)
				if mode == "expired-before-cancel" {
					if delay := time.Until(deadline.Add(15 * time.Millisecond)); delay > 0 {
						time.Sleep(delay)
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"id": c.BillingOperationID, "status": "refunded", "prepaidMicros": 300000, "chargedMicros": 0, "refundMicros": 300000}})
			})
			processOneAsyncUsage(t.Context(), info)
			require.EqualValues(t, 1, statusCalls.Load())
			require.EqualValues(t, 2, walletCalls.Load())
			require.Zero(t, redirectCalls.Load())
			expected := int32(1)
			if mode == "valid-fallback" {
				expected = 2
			}
			if mode == "expired-before-cancel" {
				expected = 0
			}
			require.Equal(t, expected, cancelCalls.Load())
			rows := taskPollRows(t, db)
			require.Len(t, rows, 1)
			if strings.HasPrefix(mode, "expired") {
				require.Equal(t, "active", rows[0].ExecutionState)
			} else {
				require.Equal(t, "error_returned", rows[0].ExecutionState)
			}
			fresh, e := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
			require.NoError(t, e)
			require.True(t, fresh.BillingSettled)
			require.Equal(t, "failed", fresh.Status)
			siblings := taskImageHandoffs(t, db)
			require.Len(t, siblings, 1)
			require.Equal(t, "exited", siblings[0].State)
		})
	}
}

func TestImagePollExecutorSettledDoesNotCloseWalletSibling(t *testing.T) {
	db, task, info := immediatePrepaidFixture(t, "completed")
	installTaskImagePoll(t, db)
	won, e := model.ClaimImagePrepaymentRecovery(t.Context(), task, time.Now())
	require.NoError(t, e)
	require.True(t, won)
	handoff, e := model.PrepareImageRecoveryHandoff(t.Context(), task)
	require.NoError(t, e)
	require.NotNil(t, handoff)
	ok, e := handoff.Accept(t.Context())
	require.NoError(t, e)
	require.True(t, ok)
	sibling := taskImageHandoffs(t, db)[0]
	owner := taskImageClaim(t, db)
	require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Updates(map[string]any{"billing_settled": true, "billing": `{"currency":"USD","status":"settled","prepaidMicros":300000,"actualMicros":400000,"refundMicros":0}`}).Error)
	claimPollFixture(t, db, info, "settled-poll", time.Minute)
	var calls atomic.Int32
	imageRecoveryWallet(t, func(http.ResponseWriter, *http.Request, balance.PrepaymentCommand) { calls.Add(1) })
	processOneAsyncUsage(t.Context(), info)
	require.Zero(t, calls.Load())
	require.Equal(t, "exited", taskPollRows(t, db)[0].State)
	require.Equal(t, sibling, taskImageHandoffs(t, db)[0])
	require.Equal(t, owner, taskImageClaim(t, db))
	var uses []model.ChannelAdmissionUse
	require.NoError(t, db.Find(&uses).Error)
	for _, use := range uses {
		require.Equal(t, "open", use.State)
	}
}

// The actual archive loop uses a local synthetic object-store callback; no bucket
// or provider is accessed. Both receipts still pass through the installed journal.
func TestImagePollExecutorArchiveReceipts(t *testing.T) {
	for _, mode := range []string{"live", "expired", "owner"} {
		t.Run(mode, func(t *testing.T) {
			db, task, info := immediatePrepaidFixture(t, "in_progress")
			require.NoError(t, model.SetImageTaskResult(task.ID, "result_processing", []model.ImageOutput{{URL: "https://cdn.example/1.png"}, {URL: "https://cdn.example/2.png"}}, nil))
			require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Updates(map[string]any{"archive_required": true, "expected_images": 2}).Error)
			installTaskImagePoll(t, db)
			claimPollFixture(t, db, info, "archive-poll", time.Minute)
			ok, err := model.AcceptImagePoll(t.Context(), info)
			require.NoError(t, err)
			require.True(t, ok)
			task, err = model.GetImageTask(task.ID, task.GroupID, task.TokenID)
			require.NoError(t, err)
			ctx := actionguard.With(t.Context(), func(ctx context.Context) error { return model.ValidateClaimedImageTask(ctx, info, task) })
			calls := 0
			err = archiveClaimedImageTask(ctx, info, task, func(context.Context, string) (string, ownedimage.Metadata, error) {
				calls++
				if mode == "expired" {
					require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("next_poll_at", time.Now().Add(-time.Second)).Error)
				}
				if mode == "owner" {
					require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("processing_token", "replacement").Error)
				}
				return "https://owned.example/image.png", ownedimage.Metadata{ContentType: "image/png"}, nil
			})
			saved, e := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
			require.NoError(t, e)
			if mode == "live" {
				require.NoError(t, err)
				require.Equal(t, 2, calls)
				require.Equal(t, "completed", saved.Status)
				require.NoError(t, model.RecordImagePollReturn(t.Context(), info))
				require.Equal(t, "exited", taskPollRows(t, db)[0].State)
			} else {
				require.Error(t, err)
				require.Equal(t, 1, calls)
				require.False(t, saved.Data[0].Stored)
				require.Error(t, model.RecordImagePollReturn(t.Context(), info))
				require.Equal(t, "accepted", taskPollRows(t, db)[0].State)
			}
		})
	}
}

func TestImagePollExecutorFirstMeasurementParksAndReturns(t *testing.T) {
	for _, mode := range []string{"park", "complete", "tampered-price", "wrong-mode", "prepaid-conflict"} {
		t.Run(mode, func(t *testing.T) {
			db, task, info := immediatePrepaidFixture(t, "completed")
			if mode != "prepaid-conflict" {
				require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("prepayment_quote_json", "").Error)
			}
			info.Price = model.Price{ImageBilling: &model.ImageBillingPolicy{Version: 1, Scenario: "generation", OutputPixelTiers: []model.ImageBillingTier{{AmountMicros: 300000, UnitQuantity: 1}}}}

			if mode == "complete" {
				w := int64(10)
				inputs := int64(0)
				data, err := json.Marshal([]model.ImageOutput{{URL: "https://example.invalid/image.png", Width: &w, Height: &w}})
				require.NoError(t, err)
				require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("data", string(data)).Error)
				info.UsageContext = model.UsageContext{ImageUsage: &model.ImageUsage{Version: 1, State: "incomplete", Scenario: "generation", InputCount: &inputs, Outputs: []model.ImageUsageOutput{}}}
			}
			if mode == "prepaid-conflict" {
				info.MeasuredImage = true
			}
			require.NoError(t, db.Save(info).Error)
			consumer := &pricingCaptureConsumer{}
			old := balance.Default
			balance.Default = pricingCaptureBalance{consumer: consumer}
			t.Cleanup(func() { balance.Default = old })
			require.NoError(t, model.CacheSetGroup(&model.GroupCache{ID: info.GroupID, Status: model.GroupStatusEnabled}))
			t.Cleanup(func() { _ = model.CacheDeleteGroup(info.GroupID) })
			installTaskImagePoll(t, db)
			claimPollFixture(t, db, info, "first-measurement", time.Minute)
			if mode == "tampered-price" {
				info.Price = model.Price{ImageBilling: &model.ImageBillingPolicy{Version: 1, Scenario: "edit"}}
			}
			if mode == "wrong-mode" {
				info.MeasuredImage = true
			}
			processOneAsyncUsage(t.Context(), info)
			var got model.AsyncUsageInfo
			require.NoError(t, db.First(&got, info.ID).Error)
			if mode == "tampered-price" || mode == "wrong-mode" || mode == "prepaid-conflict" {
				require.Zero(t, consumer.attempts)
				require.Equal(t, model.AsyncUsageStatusPending, got.Status)
				require.Equal(t, "first-measurement", got.ProcessingToken)
				return
			}
			require.True(t, got.MeasuredImage)
			expected := model.AsyncUsageStatusMeasurementPending
			if mode == "complete" {
				expected = model.AsyncUsageStatusCompleted
				require.Equal(t, 1, consumer.attempts)
				require.InDelta(t, .3, got.Amount.UsedAmount, .000001)
			} else {
				require.Zero(t, consumer.attempts)
			}
			require.Equal(t, expected, got.Status)
			require.Empty(t, got.ProcessingToken)
			var entry model.Log
			require.NoError(t, db.First(&entry, info.LogID).Error)
			require.Equal(t, expected, entry.AsyncUsageStatus)
			require.Equal(t, "exited", taskPollRows(t, db)[0].State)
			require.Equal(t, "unproven", taskPollRows(t, db)[0].ContinuationState)
		})
	}
}
