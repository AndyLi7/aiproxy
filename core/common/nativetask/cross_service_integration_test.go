package nativetask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// Combined disposable application wallet + artifact service rehearsal. Only
// provider output/bytes/bill and one transport failure are synthetic. This tests
// recovery and accounting together, not live provider generation or admission.
func TestNativeCrossServiceArchiveRecovery(t *testing.T) {
	walletURL, artifactURL := os.Getenv("D34_TEST_WALLET_URL"), os.Getenv("NATIVE_ARTIFACT_TEST_URL")
	if walletURL == "" || artifactURL == "" {
		t.Skip("requires both disposable application fixtures")
	}
	for _, endpoint := range []string{walletURL, artifactURL} {
		require.True(t, strings.HasPrefix(endpoint, "http://127.0.0.1:"))
	}
	t.Setenv("EXTERNAL_BALANCE_URL", artifactURL)
	t.Setenv("EXTERNAL_BALANCE_KEY", "local-native-test-only")
	ctx := context.Background()
	engine, plan, _, provider := setup(t)
	wallet := balance.NewExternalHTTP(walletURL, "local-d34-test-only")
	engine.Wallet = wallet
	var contract nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(plan.Contract, &contract))
	contract.Artifacts = []nativeresult.ArtifactBinding{{Path: []string{"files", "*", "url"}}}
	plan.Contract, _ = json.Marshal(contract)
	plan.DeliveryBase = "https://gateway.example"
	const id = "cross-service-recovery"
	task, err := engine.Submit(ctx, id, "u_u", 1, body, plan)
	require.NoError(t, err)
	require.Equal(t, "queued", task.Status)
	files := [][]byte{[]byte("<svg><script>untrusted</script></svg>"), {80, 75, 3, 4, 0, 255}}
	calls := [2]int{}
	fail := true
	archive := func(ctx context.Context, taskID string, index int, source string) (ownedartifact.Receipt, error) {
		calls[index]++
		require.Equal(t, fmt.Sprintf("https://provider.example/%d", index), source)
		if index == 1 && fail {
			return ownedartifact.Receipt{}, errors.New("isolated storage interruption")
		}
		// Supply synthetic provider bytes to the real authenticated storage handler.
		req, err := http.NewRequestWithContext(ctx, "POST", artifactURL+"/api/internal/media/files", bytes.NewReader(files[index]))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer local-native-test-only")
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("X-Native-Task-ID", taskID)
		req.Header.Set("X-Native-Artifact-Index", fmt.Sprint(index))
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer response.Body.Close()
		require.Equal(t, 200, response.StatusCode)
		var result struct {
			Code int                   `json:"code"`
			Data ownedartifact.Receipt `json:"data"`
		}
		require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
		require.Zero(t, result.Code)
		require.True(t, ownedartifact.ValidReceipt(taskID, index, result.Data))
		return result.Data, nil
	}
	poll := &pollStub{result: nativeresult.PollResult{Status: "result_received", Output: json.RawMessage(`{"files":[{"url":"https://provider.example/0"},{"url":"https://provider.example/1"}],"seed":9007199254740993}`)}}
	_, err = engine.Poll(ctx, id, "u_u", 1, func(context.Context, *model.NativeTask) (Poller, error) { return poll, nil }, archive)
	require.Error(t, err)
	saved, err := model.GetNativeTask(engine.DB, id, "u_u", 1)
	require.NoError(t, err)
	require.Equal(t, "result_received", saved.Status)
	require.Empty(t, saved.DeliveredOutput)
	require.False(t, saved.BillingSettled)
	var receipts map[int]ownedartifact.Receipt
	require.NoError(t, json.Unmarshal([]byte(saved.ArtifactManifest), &receipts))
	require.Len(t, receipts, 1)
	// Reopen durable state after partial archival; the first file must not repeat.
	var databases []struct{ File string }
	require.NoError(t, engine.DB.Raw("PRAGMA database_list").Scan(&databases).Error)
	conn, err := engine.DB.DB()
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	reopened, err := model.OpenSQLite(databases[0].File)
	require.NoError(t, err)
	t.Cleanup(func() { conn, _ := reopened.DB(); _ = conn.Close() })
	engine = &Engine{DB: reopened, Wallet: wallet, Provider: provider}
	fail = false
	done, err := engine.Deliver(ctx, id, "u_u", 1, archive)
	require.NoError(t, err)
	require.Equal(t, "completed", done.Status)
	require.Equal(t, [2]int{1, 2}, calls)
	require.Contains(t, done.DeliveredOutput, "9007199254740993")
	require.NotContains(t, done.DeliveredOutput, "provider.example")
	require.False(t, done.BillingSettled, "delivery is not proof of an actual settled bill")
	postWallet := func(path string, value any) json.RawMessage {
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		req, err := http.NewRequest("POST", walletURL+path, bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer local-d34-test-only")
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer response.Body.Close()
		var result struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		}
		require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
		require.Zero(t, result.Code, result.Message)
		return result.Data
	}
	postWallet("/_test/bill", map[string]any{"provider": "fal", "requestId": task.UpstreamID, "endpoint": plan.Endpoint, "currency": "USD", "subtotal": "0.0001", "total": "0.00008"})
	for i := 0; i < 2; i++ {
		require.NoError(t, wallet.RecoverPrepayments(ctx))
		require.NoError(t, engine.SyncBilling(ctx, done))
	}
	require.True(t, done.BillingSettled)
	var receipt balance.PrepaymentReceipt
	require.NoError(t, json.Unmarshal([]byte(done.BillingReceiptJSON), &receipt))
	require.NotNil(t, receipt.ChargedMicros)
	require.Equal(t, int64(80), *receipt.ChargedMicros)
	_, err = engine.Submit(ctx, id, "u_u", 1, body, plan)
	require.NoError(t, err)
	require.Equal(t, 1, provider.calls)
	var rows []struct {
		RequestID string `json:"request_id"`
		Amount    int64  `json:"amount_micros"`
	}
	require.NoError(t, json.Unmarshal(postWallet("/_test/ledger", nil), &rows))
	count, net := 0, int64(0)
	for _, row := range rows {
		if row.RequestID == "prepay:native:"+id || row.RequestID == "prepay-refund:native:"+id {
			count++
			net += row.Amount
		}
	}
	require.Equal(t, 2, count)
	require.Equal(t, int64(-80), net)
	token := 1
	h := &HTTP{Engine: engine, Identity: func(*http.Request) (string, int, error) { return "u_u", token, nil }, Download: func(r *http.Request, key string) (*http.Response, error) {
		return ownedartifact.Download(r.Context(), key)
	}}
	for index, data := range files {
		response := httptest.NewRecorder()
		h.GetArtifact(response, httptest.NewRequest("GET", "/", nil), id, fmt.Sprint(index))
		require.Equal(t, 200, response.Code, response.Body.String())
		require.Equal(t, data, response.Body.Bytes())
		require.Equal(t, "application/octet-stream", response.Header().Get("Content-Type"))
		require.Contains(t, response.Header().Get("Content-Disposition"), "attachment")
		require.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
	}
	token = 2
	hidden := httptest.NewRecorder()
	h.GetArtifact(hidden, httptest.NewRequest("GET", "/", nil), id, "0")
	require.Equal(t, 404, hidden.Code)
	require.Equal(t, [2]int{1, 2}, calls)
}
