package balance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrepaymentRetryKeepsServerIdentityAndNeverUsesBalanceCache(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "/api/internal/wallet/prepayment", r.URL.Path)
		require.Equal(t, "Bearer private-test-key", r.Header.Get("Authorization"))
		var command PrepaymentCommand
		require.NoError(t, json.NewDecoder(r.Body).Decode(&command))
		require.Equal(t, "op-1", command.BillingOperationID)
		require.NotNil(t, command.PrepaidMicros)
		require.Zero(t, *command.PrepaidMicros)
		_, _ = w.Write([]byte(`{"code":0,"data":{"id":"op-1","status":"pending"}}`))
	}))
	defer server.Close()
	client := &ExternalHTTP{url: server.URL, key: "private-test-key"}
	zero := int64(0)
	cmd := PrepaymentCommand{Action: "admit", Group: "user", BillingOperationID: "op-1", PrepaidMicros: &zero}
	for range 2 {
		receipt, err := client.Prepayment(context.Background(), cmd)
		require.NoError(t, err)
		require.Equal(t, "op-1", receipt.ID)
	}
	require.Equal(t, 2, calls)
}

func TestPrepaymentRejectsAmbiguousOrWrongIdentityReceiptsAndRedirects(t *testing.T) {
	for _, body := range []string{`{}`, `{"code":1,"data":{"id":"op-1"}}`, `{"code":0,"data":{"id":"other"}}`, `not-json`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		client := &ExternalHTTP{url: server.URL, key: "private-test-key"}
		_, err := client.Prepayment(context.Background(), PrepaymentCommand{Action: "get", Group: "user", BillingOperationID: "op-1"})
		require.Error(t, err)
		server.Close()
	}
	leaked := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked = true }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 307) }))
	defer server.Close()
	client := &ExternalHTTP{url: server.URL, key: "private-test-key"}
	_, err := client.Prepayment(context.Background(), PrepaymentCommand{Action: "get", Group: "user", BillingOperationID: "op-1"})
	require.Error(t, err)
	require.False(t, leaked)
}

func TestPrepaymentActualCostReceiptRequiresFrozenPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, policy, status string
		charge, refund       int64
		ok                   bool
	}{
		{"legacy capped", "", "settled", 80, 120, true},
		{"legacy excess", "", "settled", 400, 0, false},
		{"actual excess", "actual-cost-v1", "settled", 400, 0, true},
		{"actual refund", "actual-cost-v1", "settled", 80, 120, true},
		{"wrong refund", "actual-cost-v1", "settled", 400, 10, false},
		{"unsafe amount", "actual-cost-v1", "settled", 9007199254740992, 0, false},
		{"estimated actual", "actual-cost-v1", "estimated", 80, 120, false},
		{"unknown policy", "unknown", "settled", 80, 120, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quote, _ := json.Marshal(map[string]string{"settlementPolicy": tc.policy})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"id": "op-1", "status": tc.status, "prepaidMicros": 200, "chargedMicros": tc.charge, "refundMicros": tc.refund, "quoteJson": string(quote)}})
			}))
			defer server.Close()
			client := &ExternalHTTP{url: server.URL, key: "private-test-key"}
			_, err := client.Prepayment(context.Background(), PrepaymentCommand{Action: "get", Group: "user", BillingOperationID: "op-1"})
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
