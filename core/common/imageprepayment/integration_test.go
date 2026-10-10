package imageprepayment

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestRealWalletPrepaymentLifecycle(t *testing.T) {
	endpoint := os.Getenv("D34_TEST_WALLET_URL")
	if endpoint == "" {
		t.Skip("start companion prepayment-wallet-server.ts loopback fixture")
	}
	require.True(t, strings.HasPrefix(endpoint, "http://127.0.0.1:"))
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "gateway.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))
	oldDB, oldBalance := model.LogDB, balance.Default
	model.LogDB = db
	balance.Default = balance.NewExternalHTTP(endpoint, "local-d34-test-only")
	t.Cleanup(func() { model.LogDB = oldDB; balance.Default = oldBalance })
	post := func(path string, value any) json.RawMessage {
		encoded, err := json.Marshal(value)
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPost, endpoint+path, bytes.NewReader(encoded))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer local-d34-test-only")
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer response.Body.Close()
		var result struct {
			Code    int             `json:"code"`
			Data    json.RawMessage `json:"data"`
			Message string          `json:"message"`
		}
		require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
		require.Equal(t, 0, result.Code, result.Message)
		return result.Data
	}
	makeTask := func(id string) *model.ImageTask {
		task := &model.ImageTask{ID: id, Model: "d34-test", GroupID: "u_u", BillingOperationID: "server-" + id, Status: "submitting", UpstreamModel: "fal-ai/test", PrepaymentQuoteJSON: `{"version":1,"quoteVersion":"reviewed-test-1","currency":"USD","prepaidMicros":600000,"routes":[{"routeId":"primary","channelId":7,"provider":"fal","endpoint":"fal-ai/test","credentialScope":"channel-7","estimatedMicros":400000,"prepaidMicros":600000,"rule":{"mode":"list_ratio","ratio":"1"}}]}`}
		require.NoError(t, db.Create(task).Error)
		return task
	}
	ctx := context.Background()
	task := makeTask("completed-case")
	require.NoError(t, Admit(ctx, task, 7))
	require.NoError(t, Admit(ctx, task, 7))
	claimed, err := Begin(ctx, task, 7, "fal-ai/test", 0)
	require.NoError(t, err)
	require.True(t, claimed)
	claimed, err = Begin(ctx, task, 7, "fal-ai/test", 0)
	require.NoError(t, err)
	require.False(t, claimed)
	task.Attempts = []model.ImageTaskAttempt{{ChannelID: 7, Failure: failover.Failure{Acceptance: failover.Accepted}}}
	task.UpstreamID = "provider-completed"
	task.Status = "completed"
	require.NoError(t, db.Save(task).Error)
	receipt, err := Sync(ctx, task)
	require.NoError(t, err)
	require.Nil(t, receipt.ChargedMicros)
	post("/_test/bill", map[string]any{"provider": "fal", "requestId": task.UpstreamID, "endpoint": "fal-ai/test", "currency": "USD", "subtotal": "0.3", "total": "0.2"})
	client := balance.Default.(*balance.ExternalHTTP)
	require.NoError(t, client.RecoverPrepayments(ctx))
	for range 2 {
		receipt, err = Sync(ctx, task)
		require.NoError(t, err)
		require.NotNil(t, receipt.ChargedMicros)
		require.Equal(t, int64(300000), *receipt.ChargedMicros)
	}

	for _, tc := range []struct {
		id       string
		total    string
		expected int64
	}{{"invalid-free", "0", 0}, {"failed-billed", "0.2", 200000}} {
		failed := makeTask(tc.id)
		require.NoError(t, Admit(ctx, failed, 7))
		claimed, err := Begin(ctx, failed, 7, "fal-ai/test", 0)
		require.NoError(t, err)
		require.True(t, claimed)
		failed.Attempts = []model.ImageTaskAttempt{{ChannelID: 7, Failure: failover.Failure{Acceptance: failover.Accepted}}}
		failed.UpstreamID = "provider-" + tc.id
		failed.Status = "failed"
		failed.Error = &model.ImageTaskError{Code: "invalid_parameters"}
		require.NoError(t, db.Save(failed).Error)
		receipt, err := Sync(ctx, failed)
		require.NoError(t, err)
		require.Nil(t, receipt.ChargedMicros)
		require.NotEqual(t, "refunded", receipt.Status)
		post("/_test/bill", map[string]any{"provider": "fal", "requestId": failed.UpstreamID, "endpoint": "fal-ai/test", "currency": "USD", "subtotal": tc.total, "total": tc.total})
		require.NoError(t, client.RecoverPrepayments(ctx))
		for range 2 {
			receipt, err = Sync(ctx, failed)
			require.NoError(t, err)
			require.NotNil(t, receipt.ChargedMicros)
			require.Equal(t, tc.expected, *receipt.ChargedMicros)
		}
	}
	unknown := makeTask("unknown-case")
	require.NoError(t, Admit(ctx, unknown, 7))
	claimed, err = Begin(ctx, unknown, 7, "fal-ai/test", 0)
	require.NoError(t, err)
	require.True(t, claimed)
	post("/_test/advance", map[string]any{"ms": 86400001})
	require.NoError(t, client.RecoverPrepayments(ctx))
	receipt, err = Sync(ctx, unknown)
	require.NoError(t, err)
	require.Equal(t, "refunded", receipt.Status)
	require.Equal(t, "failed", unknown.Status)
	var rows []struct {
		RequestID string `json:"request_id"`
		Amount    int64  `json:"amount_micros"`
	}
	require.NoError(t, json.Unmarshal(post("/_test/ledger", nil), &rows))
	for _, tc := range []struct {
		id  string
		net int64
	}{{"server-completed-case", -300000}, {"server-unknown-case", 0}, {"server-invalid-free", 0}, {"server-failed-billed", -200000}} {
		count := 0
		net := int64(0)
		for _, row := range rows {
			if row.RequestID == "prepay:"+tc.id || row.RequestID == "prepay-refund:"+tc.id {
				count++
				net += row.Amount
			}
		}
		require.Equal(t, 2, count)
		require.Equal(t, tc.net, net)
	}
}
