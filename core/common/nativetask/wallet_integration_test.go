package nativetask

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/stretchr/testify/require"
)

// Companion application fixture uses the real wallet command/reconciliation
// services with disposable SQLite. Provider generation is still stubbed.
func TestNativeRealWalletSettlementAndReplay(t *testing.T) {
	endpoint := os.Getenv("D34_TEST_WALLET_URL")
	if endpoint == "" {
		t.Skip("requires disposable prepayment-wallet-server.ts")
	}
	require.True(t, strings.HasPrefix(endpoint, "http://127.0.0.1:"))
	e, plan, _, provider := setup(t)
	wallet := balance.NewExternalHTTP(endpoint, "local-d34-test-only")
	e.Wallet = wallet
	ctx := context.Background()
	id := "native-real-wallet"
	post := func(path string, value any) json.RawMessage {
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		req, err := http.NewRequest("POST", endpoint+path, bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer local-d34-test-only")
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer response.Body.Close()
		var out struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		}
		require.NoError(t, json.NewDecoder(response.Body).Decode(&out))
		require.Equal(t, 0, out.Code, out.Message)
		return out.Data
	}
	task, err := e.Submit(ctx, id, "u_u", 1, body, plan)
	require.NoError(t, err)
	require.Equal(t, "queued", task.Status)
	require.Equal(t, 1, provider.calls)
	_, err = e.Submit(ctx, id, "u_u", 1, body, plan)
	require.NoError(t, err)
	require.Equal(t, 1, provider.calls)
	poll := &pollStub{result: nativeresult.PollResult{Status: "result_received", Output: json.RawMessage(`{"text":"native result"}`)}}
	done, err := e.Poll(ctx, id, "u_u", 1, func(context.Context, *model.NativeTask) (Poller, error) { return poll, nil }, nil)
	require.NoError(t, err)
	require.Equal(t, "completed", done.Status)
	var pending balance.PrepaymentReceipt
	require.NoError(t, json.Unmarshal([]byte(done.BillingReceiptJSON), &pending))
	require.Nil(t, pending.ChargedMicros, "delivered output is not a settled provider bill")
	post("/_test/bill", map[string]any{"provider": "fal", "requestId": task.UpstreamID, "endpoint": plan.Endpoint, "currency": "USD", "subtotal": "0.0001", "total": "0.00008"})
	require.NoError(t, wallet.RecoverPrepayments(ctx))
	for i := 0; i < 2; i++ {
		require.NoError(t, e.SyncBilling(ctx, done))
	}
	var settled balance.PrepaymentReceipt
	require.NoError(t, json.Unmarshal([]byte(done.BillingReceiptJSON), &settled))
	require.NotNil(t, settled.ChargedMicros)
	require.Equal(t, int64(80), *settled.ChargedMicros)
	saved, err := model.GetNativeTask(e.DB, id, "u_u", 1)
	require.NoError(t, err)
	require.True(t, saved.BillingSettled)
	var rows []struct {
		RequestID string `json:"request_id"`
		Amount    int64  `json:"amount_micros"`
	}
	require.NoError(t, json.Unmarshal(post("/_test/ledger", nil), &rows))
	count, net := 0, int64(0)
	for _, row := range rows {
		if row.RequestID == "prepay:native:"+id || row.RequestID == "prepay-refund:native:"+id {
			count++
			net += row.Amount
		}
	}
	require.Equal(t, 2, count)
	require.Equal(t, int64(-80), net)
	require.Equal(t, 1, provider.calls)
}

func TestNativeRealWalletRejectedSubmissionRefund(t *testing.T) {
	endpoint := os.Getenv("D34_TEST_WALLET_URL")
	if endpoint == "" {
		t.Skip("requires disposable prepayment-wallet-server.ts")
	}
	require.True(t, strings.HasPrefix(endpoint, "http://127.0.0.1:"))
	engine, plan, _, provider := setup(t)
	provider.err = adaptor.ErrImageSubmissionRejected
	engine.Wallet = balance.NewExternalHTTP(endpoint, "local-d34-test-only")
	task, err := engine.Submit(context.Background(), "native-real-rejected", "u_u", 1, body, plan)
	require.NoError(t, err)
	require.Equal(t, "failed", task.Status)
	for i := 0; i < 2; i++ {
		require.NoError(t, engine.SyncBilling(context.Background(), task))
	}
	var receipt balance.PrepaymentReceipt
	require.NoError(t, json.Unmarshal([]byte(task.BillingReceiptJSON), &receipt))
	require.NotNil(t, receipt.ChargedMicros)
	require.Equal(t, int64(0), *receipt.ChargedMicros)
	require.True(t, task.BillingSettled)
	require.Equal(t, 1, provider.calls)
}
