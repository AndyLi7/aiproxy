package task

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func measuredWorkerFixture(t *testing.T, amount float64, groups ...string) (*gorm.DB, *model.AsyncUsageInfo) {
	t.Helper()
	group := "g"
	if len(groups) > 0 {
		group = groups[0]
	}
	db, _, _ := imageRecoveryRuntime(t)
	require.NoError(t, db.AutoMigrate(&model.ChannelAdmissionExecution{}, &model.ChannelAdmissionExecutionProtocol{}, &model.ChannelAdmissionMeasuredProducer{}, &model.ChannelAdmissionMeasuredClaim{}, &model.Group{}, &model.ConsumeError{}))
	require.NoError(t, db.Save(&model.ChannelAdmissionExecutionProtocol{ID: 1, Version: 1}).Error)
	require.NoError(t, db.Save(&model.ChannelAdmissionExecutionProtocol{ID: 9, Version: 1}).Error)
	require.NoError(t, db.Create(&model.Group{ID: group, Status: model.GroupStatusEnabled}).Error)
	require.NoError(t, model.CacheSetGroup(&model.GroupCache{ID: group, Status: model.GroupStatusEnabled}))
	t.Cleanup(func() { _ = model.CacheDeleteGroup(group) })
	var ch model.Channel
	require.NoError(t, db.First(&ch, 1).Error)
	at := time.Now()
	price := model.Price{OutputPrice: .3, OutputPriceUnit: 1, ImageBilling: &model.ImageBillingPolicy{Version: 1, Scenario: "generation"}}
	intent := model.NewMeasuredRelayIntent(price, at, false)
	_, ex, e := model.AdmitRelayExecution(t.Context(), &ch, model.RelayAdmissionScope{GroupID: group, TokenID: 1, RequestID: "measured-worker", BillingOperationID: "measured-worker-op", Model: "image"}, intent)
	require.NoError(t, e)
	require.NoError(t, ex.RecordReturn(t.Context(), model.RelayExecutionReturned))
	micros := int64(amount * 1e6)
	u := &model.AsyncUsageInfo{GroupID: group, TokenID: 1, RequestID: "measured-worker", BillingOperationID: "measured-worker-op", Model: "image", ChannelID: 1, RequestAt: at, PricingCurrency: "USD", PricingVersion: "measured-frozen-v1", Price: price, Amount: model.Amount{UsedAmount: amount, ImageBillingResult: &model.ImageBillingResult{State: "complete", AmountMicros: &micros}}}
	entry := &model.Log{GroupID: group, TokenID: 1, RequestID: "measured-worker", BillingOperationID: "measured-worker-op", ChannelID: 1, RequestAt: at, Currency: u.PricingCurrency, PricingVersion: u.PricingVersion}
	require.NoError(t, model.CreateMeasuredImageSettlementOwned(t.Context(), intent, price, entry, u))
	require.NoError(t, model.RecordMeasuredProducerReturn(t.Context(), intent))
	return db, u
}
func measuredTaskRows(t *testing.T, db *gorm.DB) []model.ChannelAdmissionMeasuredClaim {
	var rows []model.ChannelAdmissionMeasuredClaim
	require.NoError(t, db.Order("offered_at_ms,id").Find(&rows).Error)
	return rows
}
func TestMeasuredHandoffActualWorker(t *testing.T) {
	for _, scenario := range []string{"positive", "zero", "expired-entry", "new-owner", "accepted-lost", "panic", "goexit", "return-failure"} {
		t.Run(scenario, func(t *testing.T) {
			amount := .3
			if scenario == "zero" {
				amount = 0
			}
			db, u := measuredWorkerFixture(t, amount)
			claimPollFixture(t, db, u, "owner", time.Minute)
			calls, debits, charges := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/internal/wallet/balance" {
					_, _ = w.Write([]byte(`{"code":0,"data":{"balance":100}}`))
					return
				}
				debits++
				if charges == 0 {
					charges++
				}
				if scenario == "accepted-lost" && debits == 1 {
					require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", u.ID).Update("next_poll_at", time.Now().Add(-time.Second)).Error)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"balance": 99.7, "charged": .3, "receipt": "created"}})
			}))
			defer server.Close()
			balance.Default = balance.NewExternalHTTP(server.URL, "local-fixture")
			if scenario == "expired-entry" {
				require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", u.ID).Update("next_poll_at", time.Now().Add(-time.Second)).Error)
			}
			if scenario == "new-owner" {
				require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", u.ID).Update("processing_token", "other").Error)
			}
			if scenario == "panic" || scenario == "goexit" {
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("measured:panic", func(tx *gorm.DB) {
					if tx.Statement.Table == "logs" {
						if scenario == "panic" {
							panic("synthetic accounting panic")
						}
						runtime.Goexit()
					}
				}))
			}
			if scenario == "return-failure" {
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("measured:return", func(tx *gorm.DB) {
					v, _ := tx.Statement.Dest.(map[string]any)
					if tx.Statement.Table == "channel_admission_measured_claims" && v["state"] == "exited" {
						tx.AddError(context.Canceled)
					}
				}))
			}
			if scenario == "panic" {
				require.Panics(t, func() { processOneAsyncUsage(t.Context(), u) })
			} else if scenario == "goexit" {
				done := make(chan struct{})
				go func() { defer close(done); processOneAsyncUsage(t.Context(), u) }()
				<-done
			} else {
				processOneAsyncUsage(t.Context(), u)
			}
			rows := measuredTaskRows(t, db)
			require.Len(t, rows, 1)
			switch scenario {
			case "expired-entry", "new-owner":
				require.Zero(t, calls)
				require.Equal(t, "offered", rows[0].State)
			case "panic", "goexit":
				require.Zero(t, calls)
				require.Equal(t, "accepted", rows[0].State)
			case "return-failure":
				require.Equal(t, "accepted", rows[0].State)
				require.NotEmpty(t, rows[0].ReleaseHash)
			case "accepted-lost":
				require.Equal(t, 1, debits)
				require.Equal(t, "accepted", rows[0].State)
				old := rows[0]
				var fresh model.AsyncUsageInfo
				require.NoError(t, db.First(&fresh, u.ID).Error)
				require.False(t, fresh.BalanceConsumed)
				claimPollFixture(t, db, &fresh, "next", time.Minute)
				processOneAsyncUsage(t.Context(), &fresh)
				rows = measuredTaskRows(t, db)
				require.Len(t, rows, 2)
				require.Equal(t, old, rows[0])
				require.Equal(t, "exited", rows[1].State)
				require.Equal(t, 2, debits)
				require.Equal(t, 1, charges)
			default:
				require.Equal(t, "exited", rows[0].State)
				require.NotEmpty(t, rows[0].ReleaseHash)
				require.Equal(t, 1, debits)
			}
		})
	}
}

func TestMeasuredHandoffRealWalletReplay(t *testing.T) {
	walletURL := os.Getenv("ACCOUNTING_TEST_WALLET_URL")
	if walletURL == "" {
		t.Skip("requires isolated application wallet fixture")
	}
	target, e := url.Parse(walletURL)
	require.NoError(t, e)
	require.Equal(t, "127.0.0.1", target.Hostname())
	db, u := measuredWorkerFixture(t, .3, "u_user_1")
	claimPollFixture(t, db, u, "first", time.Minute)
	var receipts atomic.Int32
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.Request.URL.Path == "/api/internal/wallet/consume" && receipts.Add(1) == 1 {
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("fixture wallet rejected consume: %d", resp.StatusCode)
			}
			return db.Model(&model.AsyncUsageInfo{}).Where("id=?", u.ID).Update("next_poll_at", time.Now().Add(-time.Second)).Error
		}
		return nil
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	balance.Default = balance.NewExternalHTTP(server.URL, "local-sec-test-only")
	processOneAsyncUsage(t.Context(), u)
	var fresh model.AsyncUsageInfo
	require.NoError(t, db.First(&fresh, u.ID).Error)
	require.False(t, fresh.BalanceConsumed)
	require.Equal(t, model.AsyncUsageStatusPending, fresh.Status)
	old := measuredTaskRows(t, db)[0]
	require.Equal(t, "accepted", old.State)
	claimPollFixture(t, db, &fresh, "second", time.Minute)
	processOneAsyncUsage(t.Context(), &fresh)
	rows := measuredTaskRows(t, db)
	require.Len(t, rows, 2)
	require.Equal(t, old, rows[0])
	require.Equal(t, "exited", rows[1].State)
	require.Equal(t, int32(2), receipts.Load())
	req, e := http.NewRequest(http.MethodGet, walletURL+"/_test/ledger", nil)
	require.NoError(t, e)
	req.Header.Set("Authorization", "Bearer local-sec-test-only")
	resp, e := http.DefaultClient.Do(req)
	require.NoError(t, e)
	defer resp.Body.Close()
	var ledger []struct {
		Source string `json:"source_request_id"`
		Amount int64  `json:"amount_micros"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&ledger))
	require.Len(t, ledger, 1)
	require.Equal(t, int64(-300000), ledger[0].Amount)
	require.Equal(t, u.RequestID, ledger[0].Source)
}
