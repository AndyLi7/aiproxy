package task

import (
	"bytes"
	"encoding/json"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/imageprepayment"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func immediatePrepaidFixture(t *testing.T, status string) (*gorm.DB, *model.ImageTask, *model.AsyncUsageInfo) {
	db, _, identity := imageRecoveryRuntime(t)
	installTaskImageRecoveryHandoff(t, db)
	task := &model.ImageTask{ID: "immediate", Model: "synthetic/image", GroupID: "g", TokenID: 1, Fingerprint: strings.Repeat("a", 64), BillingOperationID: "billing-immediate", KeyFingerprint: identity.KeyDigest, AdmissionIdentityHash: identity.IdentityHash, ChannelType: model.ChannelTypeFal, UpstreamModel: "synthetic", PrepaymentQuoteJSON: `{"version":1,"settlementPolicy":"actual-cost-v1","quoteVersion":"owned-test","currency":"USD","prepaidMicros":300000,"routes":[{"routeId":"primary","channelId":1,"provider":"fal","endpoint":"synthetic","credentialScope":"local-fixture","estimatedMicros":300000,"prepaidMicros":300000,"rule":{"mode":"list_ratio","ratio":"1"}}]}`}
	_, created, err := model.ReserveImageTask(task, &model.AsyncUsageInfo{RequestID: task.ID, ChannelID: 1, BaseURL: "https://example.invalid", GroupID: task.GroupID, TokenID: task.TokenID, BillingOperationID: task.BillingOperationID})
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Updates(map[string]any{"status": status, "completed_at": time.Now().UTC().Add(-2 * time.Hour)}).Error)
	require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("image_task_id=?", task.ID).Updates(map[string]any{"status": model.AsyncUsageStatusPending, "processing_token": "original-poll", "next_poll_at": time.Now().Add(time.Minute), "billing_operation_id": task.BillingOperationID}).Error)
	if status == "completed" {
		require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("data", `[{"url":"https://example.invalid/image.png"}]`).Error)
	}
	task, err = model.GetImageTask(task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)
	var info model.AsyncUsageInfo
	require.NoError(t, db.First(&info, task.UsageID).Error)
	require.NoError(t, db.Model(&model.Log{}).Where("id=?", info.LogID).Update("billing_operation_id", task.BillingOperationID).Error)
	return db, task, &info
}

func TestPrepaidImmediateTerminalOwnedHandoff(t *testing.T) {
	for _, mode := range []string{"valid", "future-empty", "active-owner", "expired-nonempty", "poll-expired", "poll-new-owner", "late-wallet-owner", "partial"} {
		t.Run(mode, func(t *testing.T) {
			db, task, info := immediatePrepaidFixture(t, "failed")
			if mode == "future-empty" || mode == "active-owner" || mode == "expired-nonempty" {
				won, err := model.ClaimImagePrepaymentRecovery(t.Context(), task, time.Now())
				require.NoError(t, err)
				require.True(t, won)
				if mode == "future-empty" {
					require.NoError(t, model.ReleaseImagePrepaymentRecovery(t.Context(), task))
				}
				if mode == "expired-nonempty" {
					require.NoError(t, db.Model(&model.ChannelAdmissionImageRecoveryClaim{}).Where("1=1").Update("until_micros", time.Now().Add(-time.Second).UnixMicro()).Error)
				}
			}
			switch mode {
			case "poll-expired":
				require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("next_poll_at", time.Now().Add(-time.Second)).Error)
			case "poll-new-owner":
				require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("processing_token", "new-owner").Error)
			case "partial":
				require.NoError(t, db.Where("id=5").Delete(&model.ChannelAdmissionExecutionProtocol{}).Error)
			}
			var calls atomic.Int32
			imageRecoveryWallet(t, func(w http.ResponseWriter, r *http.Request, c balance.PrepaymentCommand) {
				calls.Add(1)
				require.Equal(t, task.BillingOperationID, c.BillingOperationID)
				if mode == "late-wallet-owner" {
					require.NoError(t, db.Model(&model.ChannelAdmissionImageRecoveryClaim{}).Where("1=1").Update("owner", strings.Repeat("b", 64)).Error)
				}
				refundedImageReceipt(w, c)
			})
			processOneImageUsage(t.Context(), info)
			fresh, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
			require.NoError(t, err)
			rows := taskImageHandoffs(t, db)
			switch mode {
			case "valid", "future-empty":
				require.EqualValues(t, 2, calls.Load())
				require.True(t, fresh.BillingSettled)
				require.Len(t, rows, 1)
				require.Equal(t, "exited", rows[0].State)
				require.Empty(t, taskImageClaim(t, db).Owner)
			case "late-wallet-owner":
				require.EqualValues(t, 1, calls.Load())
				require.False(t, fresh.BillingSettled)
				require.Len(t, rows, 1)
				require.Equal(t, "accepted", rows[0].State)
				require.Equal(t, strings.Repeat("b", 64), taskImageClaim(t, db).Owner)
			default:
				require.Zero(t, calls.Load())
				require.False(t, fresh.BillingSettled)
				require.Empty(t, rows)
			}
			// Neither the original worker nor a copy can start that terminal handoff twice.
			refundFailedImageTask(t.Context(), info, fresh)
			if mode == "valid" || mode == "future-empty" {
				require.EqualValues(t, 2, calls.Load())
			}
		})
	}
}

func TestPrepaidImmediateCompletedUsesOwnedRunner(t *testing.T) {
	for _, mode := range []string{"valid", "future", "expired", "same-token-renewed"} {
		t.Run(mode, func(t *testing.T) {
			db, task, info := immediatePrepaidFixture(t, "completed")
			if mode == "future" {
				require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("billing_next_check_at", time.Now().Add(time.Minute)).Error)
			}
			if mode == "expired" {
				require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("next_poll_at", time.Now().Add(-time.Second)).Error)
			}
			if mode == "same-token-renewed" {
				info.NextPollAt = time.Now().Add(-time.Hour)
			}
			var calls atomic.Int32
			imageRecoveryWallet(t, func(w http.ResponseWriter, r *http.Request, c balance.PrepaymentCommand) {
				calls.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"id": c.BillingOperationID, "status": "settled", "chargedMicros": 300000, "prepaidMicros": 300000, "refundMicros": 0}})
			})
			processOneImageUsage(t.Context(), info)
			if mode == "valid" || mode == "same-token-renewed" {
				require.EqualValues(t, 2, calls.Load())
				require.Len(t, taskImageHandoffs(t, db), 1)
				require.Equal(t, "exited", taskImageHandoffs(t, db)[0].State)
			} else {
				require.Zero(t, calls.Load())
				require.Empty(t, taskImageHandoffs(t, db))
			}
		})
	}
}

func TestPrepaidImmediateTerminalSerializesRecovery(t *testing.T) {
	db, task, info := immediatePrepaidFixture(t, "failed")
	entered := make(chan struct{})
	resume := make(chan struct{})
	var calls atomic.Int32
	imageRecoveryWallet(t, func(w http.ResponseWriter, r *http.Request, c balance.PrepaymentCommand) {
		if calls.Add(1) == 1 {
			close(entered)
			<-resume
		}
		refundedImageReceipt(w, c)
	})
	done := make(chan struct{})
	go func() { processOneImageUsage(t.Context(), info); close(done) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no terminal wallet action")
	}
	recoverImagePrepayments(t.Context())
	require.EqualValues(t, 1, calls.Load(), "scan must not start a second owner during terminal Sync")
	close(resume)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Sync did not exit")
	}
	require.EqualValues(t, 2, calls.Load())
	fresh, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)
	require.True(t, fresh.BillingSettled)
	require.Len(t, taskImageHandoffs(t, db), 1)
}

// Uses the current application wallet service and disposable SQLite. The proxy
// only delays the acknowledgement after the real refund committed.
func TestPrepaidOwnedSyncRealWalletAcceptedRefundRecovery(t *testing.T) {
	endpoint := os.Getenv("D34_TEST_WALLET_URL")
	if endpoint == "" {
		t.Skip("requires current app disposable prepayment-wallet-server.ts")
	}
	require.True(t, strings.HasPrefix(endpoint, "http://127.0.0.1:"))
	db, _, identity := imageRecoveryRuntime(t)
	installTaskImageRecoveryHandoff(t, db)
	task := &model.ImageTask{ID: "owned-real-refund", Model: "synthetic/image", GroupID: "u_u", TokenID: 1, Fingerprint: strings.Repeat("a", 64), BillingOperationID: "owned-real-refund-operation", KeyFingerprint: identity.KeyDigest, AdmissionIdentityHash: identity.IdentityHash, ChannelType: model.ChannelTypeFal, UpstreamModel: "synthetic", PrepaymentQuoteJSON: `{"version":1,"quoteVersion":"owned-fixture","currency":"USD","prepaidMicros":600000,"routes":[{"routeId":"primary","channelId":1,"provider":"fal","endpoint":"synthetic","credentialScope":"local-fixture","estimatedMicros":400000,"prepaidMicros":600000,"rule":{"mode":"list_ratio","ratio":"1"}}]}`}
	info := &model.AsyncUsageInfo{RequestID: task.ID, ChannelID: 1, BaseURL: "https://example.invalid", GroupID: task.GroupID, TokenID: task.TokenID, BillingOperationID: task.BillingOperationID}
	_, created, err := model.ReserveImageTask(task, info)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Updates(map[string]any{"status": "failed", "completed_at": time.Now()}).Error)
	require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("image_task_id=?", task.ID).Updates(map[string]any{"status": model.AsyncUsageStatusPending, "processing_token": "original-poll", "next_poll_at": time.Now().Add(time.Minute)}).Error)
	require.NoError(t, db.First(info, "image_task_id=?", task.ID).Error)
	task, err = model.GetImageTask(task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)
	balance.Default = balance.NewExternalHTTP(endpoint, "local-d34-test-only")
	require.NoError(t, imageprepayment.Admit(t.Context(), task, 1, model.UsageIdentity{}))
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var cmd balance.PrepaymentCommand
		require.NoError(t, json.Unmarshal(body, &cmd))
		require.Equal(t, task.BillingOperationID, cmd.BillingOperationID)
		req, err := http.NewRequestWithContext(r.Context(), r.Method, endpoint+r.URL.Path, bytes.NewReader(body))
		require.NoError(t, err)
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		defer resp.Body.Close()
		response, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		if calls.Add(1) == 1 {
			require.Equal(t, "settle", cmd.Action)
			require.NoError(t, db.Model(&model.ChannelAdmissionImageRecoveryClaim{}).Where("1=1").Update("until_micros", time.Now().Add(-time.Second).UnixMicro()).Error)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(response)
	}))
	defer proxy.Close()
	balance.Default = balance.NewExternalHTTP(proxy.URL, "local-d34-test-only")
	processOneImageUsage(t.Context(), info)
	require.EqualValues(t, 1, calls.Load(), "old owner must not send get after its refund reply lost the lease")
	fresh, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)
	require.False(t, fresh.BillingSettled)
	require.Nil(t, fresh.Billing)
	rows := taskImageHandoffs(t, db)
	require.Len(t, rows, 1)
	require.Equal(t, "accepted", rows[0].State)
	require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("billing_next_check_at", time.Now().UTC().Add(-time.Second)).Error)
	recoverImagePrepayments(t.Context())
	fresh, err = model.GetImageTask(task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)
	require.True(t, fresh.BillingSettled)
	require.Equal(t, "refunded", fresh.Billing.Status)
	require.EqualValues(t, 600000, *fresh.Billing.RefundMicros)
	require.EqualValues(t, 3, calls.Load())
	rows = taskImageHandoffs(t, db)
	require.Len(t, rows, 2)
	require.Equal(t, "accepted", rows[0].State, "recovery does not invent the old executor's exit")
	require.Equal(t, "exited", rows[1].State)
	req, err := http.NewRequest(http.MethodPost, endpoint+"/_test/ledger", strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer local-d34-test-only")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var result struct {
		Code int `json:"code"`
		Data []struct {
			RequestID string `json:"request_id"`
			Amount    int64  `json:"amount_micros"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Zero(t, result.Code)
	count := 0
	var net int64
	for _, row := range result.Data {
		if row.RequestID == "prepay:"+task.BillingOperationID || row.RequestID == "prepay-refund:"+task.BillingOperationID {
			count++
			net += row.Amount
		}
	}
	require.Equal(t, 2, count, "one original debit and exactly one full refund despite replay")
	require.Zero(t, net)
}

func TestPrepaidCompletedDurableReceiptResumesWithoutWallet(t *testing.T) {
	for _, mode := range []string{"settled", "over-prepaid", "refunded", "invalid-receipt", "operation-mismatch", "quote-corrupt", "prepaid-mismatch", "expired", "sync-then-lost-poll"} {
		t.Run(mode, func(t *testing.T) {
			db, task, info := immediatePrepaidFixture(t, "completed")
			charged, refund := int64(300000), int64(0)
			status := "settled"
			if mode == "over-prepaid" {
				charged = 600000
			}
			if mode == "refunded" {
				charged = 0
				refund = 300000
				status = "refunded"
			}
			if mode == "invalid-receipt" {
				refund = 1
			}
			raw, err := json.Marshal(&model.ImageTaskBilling{Currency: "USD", Status: status, PrepaidMicros: 300000, ActualMicros: &charged, RefundMicros: &refund})
			require.NoError(t, err)
			if mode != "sync-then-lost-poll" {
				require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Updates(map[string]any{"billing": string(raw), "billing_settled": true}).Error)
			}
			if mode == "quote-corrupt" {
				require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("prepayment_quote_json", `{"version":1}`).Error)
			}
			if mode == "prepaid-mismatch" {
				require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", task.ID).Update("billing", `{"currency":"USD","status":"settled","prepaid_micros":400000,"actual_micros":300000,"refund_micros":100000}`).Error)
			}
			if mode == "operation-mismatch" {
				require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("billing_operation_id", "different").Error)
				info.BillingOperationID = "different"
			}
			if mode == "expired" {
				require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("next_poll_at", time.Now().Add(-time.Second)).Error)
			}
			var calls atomic.Int32
			imageRecoveryWallet(t, func(w http.ResponseWriter, r *http.Request, c balance.PrepaymentCommand) {
				calls.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"id": c.BillingOperationID, "status": "settled", "chargedMicros": 300000, "prepaidMicros": 300000, "refundMicros": 0}})
			})
			if mode == "sync-then-lost-poll" {
				require.NoError(t, db.Callback().Update().After("gorm:update").Register("test:expire-after-billing", func(tx *gorm.DB) {
					if tx.Statement.Table == "image_tasks" {
						if v, ok := tx.Statement.Dest.(map[string]any); ok && v["billing"] != nil {
							tx.AddError(tx.Session(&gorm.Session{NewDB: true}).Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("next_poll_at", time.Now().Add(-time.Second)).Error)
						}
					}
				}))
				defer db.Callback().Update().Remove("test:expire-after-billing")
			}
			processOneImageUsage(t.Context(), info)
			var usage model.AsyncUsageInfo
			require.NoError(t, db.First(&usage, info.ID).Error)
			if mode == "sync-then-lost-poll" {
				require.Equal(t, model.AsyncUsageStatusPending, usage.Status)
				require.EqualValues(t, 2, calls.Load())
				var saved model.ImageTask
				require.NoError(t, db.First(&saved, "id=?", task.ID).Error)
				require.True(t, saved.BillingSettled)
				require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Updates(map[string]any{"processing_token": "new-poll-owner", "next_poll_at": time.Now().Add(time.Minute)}).Error)
				require.NoError(t, db.First(&usage, info.ID).Error)
				processOneImageUsage(t.Context(), &usage)
				require.NoError(t, db.First(&usage, info.ID).Error)
				require.EqualValues(t, 2, calls.Load(), "durable receipt resume sends no new wallet action")
			} else {
				require.Zero(t, calls.Load())
			}
			var entry model.Log
			require.NoError(t, db.First(&entry, info.LogID).Error)
			if mode == "invalid-receipt" || mode == "operation-mismatch" || mode == "quote-corrupt" || mode == "prepaid-mismatch" || mode == "expired" {
				require.Equal(t, model.AsyncUsageStatusPending, usage.Status)
				require.Equal(t, model.AsyncUsageStatusPending, entry.AsyncUsageStatus)
			} else {
				require.Equal(t, model.AsyncUsageStatusCompleted, usage.Status)
				require.Empty(t, usage.ProcessingToken)
				require.True(t, usage.BalanceConsumed)
				require.Equal(t, model.AsyncUsageStatusCompleted, entry.AsyncUsageStatus)
				require.Equal(t, float64(charged)/1000000, entry.Amount.UsedAmount)
			}
		})
	}
}
