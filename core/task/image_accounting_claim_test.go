//nolint:testpackage
package task

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/glebarez/sqlite"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func accountingFixture(t *testing.T, state string) (*gorm.DB, *model.AsyncUsageInfo) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.AsyncUsageInfo{}, &model.Log{}, &model.ImageTask{}, &model.ConsumeError{}))
	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })
	micros := int64(300000)
	info := &model.AsyncUsageInfo{RequestID: "accounting-claim", GroupID: "accounting-claim", TokenID: 1, MeasuredImage: true, ProcessingToken: "original", NextPollAt: time.Now().Add(time.Minute), RequestAt: time.Now(), Price: model.Price{OutputPrice: 9, OutputPriceUnit: 1}, Amount: model.Amount{UsedAmount: .3, ImageBillingResult: &model.ImageBillingResult{State: state, AmountMicros: &micros}}}
	require.NoError(t, model.CreateAsyncUsageInfo(info))
	entry := &model.Log{RequestID: model.EmptyNullString(info.RequestID), GroupID: info.GroupID, TokenID: info.TokenID, AsyncUsageStatus: model.AsyncUsageStatusPending}
	require.NoError(t, db.Create(entry).Error)
	info.LogID = entry.ID
	require.NoError(t, model.UpdateAsyncUsageInfo(info))
	require.NoError(t, model.CacheSetGroup(&model.GroupCache{ID: info.GroupID, Status: model.GroupStatusEnabled}))
	t.Cleanup(func() { _ = model.CacheDeleteGroup(info.GroupID) })
	return db, info
}
func TestAccountingWorkerExpiredOwnerDoesNotWrite(t *testing.T) {
	for _, state := range []string{"pending", "complete", "failed"} {
		t.Run(state, func(t *testing.T) {
			db, info := accountingFixture(t, state)
			require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("next_poll_at", time.Now().Add(-time.Second)).Error)
			var debits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				debits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"code":0,"data":{"balance":100,"charged":0.3}}`))
			}))
			defer server.Close()
			old := balance.Default
			balance.Default = balance.NewExternalHTTP(server.URL, "local-fixture")
			defer func() { balance.Default = old }()
			processOneAsyncUsage(context.Background(), info)
			var got model.AsyncUsageInfo
			require.NoError(t, db.First(&got, info.ID).Error)
			var entry model.Log
			require.NoError(t, db.First(&entry, info.LogID).Error)
			require.Equal(t, model.AsyncUsageStatusPending, got.Status)
			require.Equal(t, "original", got.ProcessingToken)
			require.Equal(t, model.AsyncUsageStatusPending, entry.AsyncUsageStatus)
			require.Equal(t, int32(0), debits.Load())
		})
	}
}
func TestAccountingWalletStopsAfterOwnerChange(t *testing.T) {
	for _, at := range []string{"balance", "consume"} {
		t.Run(at, func(t *testing.T) {
			db, info := accountingFixture(t, "complete")
			var consumes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/internal/wallet/balance" {
					if at == "balance" {
						_ = db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("processing_token", "new-owner").Error
					}
					_, _ = w.Write([]byte(`{"code":0,"data":{"balance":100}}`))
					return
				}
				consumes.Add(1)
				_ = db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("processing_token", "new-owner").Error
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			old := balance.Default
			balance.Default = balance.NewExternalHTTP(server.URL, "local-fixture")
			defer func() { balance.Default = old }()
			require.Error(t, completeAsyncUsage(context.Background(), info, info.Usage, info.UsageContext))
			want := int32(0)
			if at == "consume" {
				want = 1
			}
			require.Equal(t, want, consumes.Load())
			var got model.AsyncUsageInfo
			require.NoError(t, db.First(&got, info.ID).Error)
			require.Equal(t, "new-owner", got.ProcessingToken)
			require.False(t, got.BalanceConsumed)
			require.Equal(t, .3, got.Amount.UsedAmount)
		})
	}
}

// The companion is the real application consume route/service on disposable
// SQLite. No inference/provider is involved; the proxy only delays the receipt.
func TestAccountingRealWalletUnknownReceiptReplaysOriginalOperation(t *testing.T) {
	walletURL := os.Getenv("ACCOUNTING_TEST_WALLET_URL")
	if walletURL == "" {
		t.Skip("requires isolated application wallet fixture")
	}
	target, err := url.Parse(walletURL)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", target.Hostname(), "fixture must be loopback")
	db, info := accountingFixture(t, "complete")
	info.GroupID = "u_user_1"
	info.PricingCurrency = "USD"
	info.PricingVersion = "accounting-frozen-v1"
	info.BillingOperationID = "accounting-unknown-original"
	require.NoError(t, model.UpdateAsyncUsageInfo(info))
	require.NoError(t, db.Model(&model.Log{}).Where("id=?", info.LogID).Updates(map[string]any{"group_id": info.GroupID, "billing_operation_id": info.BillingOperationID}).Error)
	require.NoError(t, model.CacheSetGroup(&model.GroupCache{ID: info.GroupID, Status: model.GroupStatusEnabled}))
	defer model.CacheDeleteGroup(info.GroupID)
	var receipts atomic.Int32
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.Request.URL.Path == "/api/internal/wallet/consume" && receipts.Add(1) == 1 {
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("wallet rejected first request: %d", resp.StatusCode)
			}
			return db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("processing_token", "replacement").Error
		}
		return nil
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	old := balance.Default
	balance.Default = balance.NewExternalHTTP(server.URL, "local-sec-test-only")
	defer func() { balance.Default = old }()
	require.Error(t, completeAsyncUsage(t.Context(), info, info.Usage, info.UsageContext))
	var next model.AsyncUsageInfo
	require.NoError(t, db.First(&next, info.ID).Error)
	require.False(t, next.BalanceConsumed)
	require.Equal(t, model.AsyncUsageStatusPending, next.Status)
	require.Equal(t, .3, next.Amount.UsedAmount)
	require.Equal(t, "accounting-unknown-original", next.BillingOperationID)
	require.NoError(t, completeAsyncUsage(t.Context(), &next, model.Usage{OutputTokens: 999}, model.UsageContext{}))
	require.Equal(t, int32(2), receipts.Load())
	req, err := http.NewRequest(http.MethodGet, walletURL+"/_test/ledger", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer local-sec-test-only")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var rows []struct {
		Source string `json:"source_request_id"`
		Amount int64  `json:"amount_micros"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rows))
	require.Len(t, rows, 1)
	require.Equal(t, int64(-300000), rows[0].Amount)
	require.Equal(t, info.RequestID, rows[0].Source)
}

func TestAccountingMeasuredPrepaidModeConflict(t *testing.T) {
	db, info := accountingFixture(t, "complete")
	info.ImageTaskID = "prepaid"
	require.NoError(t, model.UpdateAsyncUsageInfo(info))
	require.NoError(t, db.Create(&model.ImageTask{ID: info.ImageTaskID, GroupID: info.GroupID, TokenID: info.TokenID, UsageID: info.ID, PrepaymentQuoteJSON: `{"frozen":true}`}).Error)
	var calls []string
	old := balance.Default
	balance.Default = zeroReceiptBalance{receipts: &calls}
	defer func() { balance.Default = old }()
	processOneAsyncUsage(t.Context(), info)
	require.Empty(t, calls)
	var got model.AsyncUsageInfo
	require.NoError(t, db.First(&got, info.ID).Error)
	require.Equal(t, model.AsyncUsageStatusPending, got.Status)
}

type accountingUnknownConsumer struct {
	calls int
	lose  func()
}

func (c *accountingUnknownConsumer) PostGroupConsume(context.Context, string, float64) (float64, error) {
	c.calls++
	c.lose()
	return .3, nil
}

type accountingUnknownBalance struct {
	balance.GroupBalance
	c *accountingUnknownConsumer
}

func (b accountingUnknownBalance) GetGroupRemainBalance(context.Context, model.GroupCache) (float64, balance.PostGroupConsumer, error) {
	return 100, b.c, nil
}
func TestAccountingNonReplayUnknownAttemptNeverRepeats(t *testing.T) {
	db, info := accountingFixture(t, "complete")
	consumer := &accountingUnknownConsumer{lose: func() {
		require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("processing_token", "new-owner").Error)
	}}
	old := balance.Default
	balance.Default = accountingUnknownBalance{c: consumer}
	defer func() { balance.Default = old }()
	require.Error(t, completeAsyncUsage(t.Context(), info, info.Usage, info.UsageContext))
	var next model.AsyncUsageInfo
	require.NoError(t, db.First(&next, info.ID).Error)
	require.True(t, next.BalanceConsumeAttempted)
	require.False(t, next.BalanceConsumed)
	require.Equal(t, model.AsyncUsageStatusPending, next.Status)
	require.ErrorIs(t, completeAsyncUsage(t.Context(), &next, next.Usage, next.UsageContext), errAsyncUsageManualSettlement)
	require.Equal(t, 1, consumer.calls)
}

func TestAccountingMeasuredZeroLossDoesNotFinalize(t *testing.T) {
	db, info := accountingFixture(t, "complete")
	zero := int64(0)
	info.Amount.UsedAmount = 0
	info.Amount.ImageBillingResult.AmountMicros = &zero
	require.NoError(t, model.UpdateAsyncUsageInfo(info))
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).Update("processing_token", "new-owner").Error
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"receipt":"created"}}`))
	}))
	defer server.Close()
	old := balance.Default
	balance.Default = balance.NewExternalHTTP(server.URL, "local-fixture")
	defer func() { balance.Default = old }()
	processOneAsyncUsage(t.Context(), info)
	var next model.AsyncUsageInfo
	require.NoError(t, db.First(&next, info.ID).Error)
	require.Equal(t, model.AsyncUsageStatusPending, next.Status)
	require.Equal(t, "new-owner", next.ProcessingToken)
	require.Equal(t, int32(1), calls.Load())
}
