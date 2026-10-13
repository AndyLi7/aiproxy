package nativetask

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

// fixturePost calls one of the wallet fixture's own endpoints and returns its
// data; the fixture answers code 0 on success.
func fixturePost(t *testing.T, endpoint, path string, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, endpoint+path, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer local-d34-test-only")
	req.Header.Set("Content-Type", "application/json")
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

// Incident 2026-10-13 against the real application wallet. The platform edge
// held begin_attempt during an application deploy, so the wallet processed it
// only after the gateway's 5-second wallet timeout: the claim was recorded,
// the gateway saw an unknown outcome, called no provider, answered 503 and
// left the task reserved. The hold used to wait 24 hours and then pause the
// model. Now recovery, after the grace, fails the task as not_started and
// sends reject_attempt and settle failed/not_accepted: the wallet row ends
// refunded with its submission not_submitted, the ledger nets zero, and a day
// later the application's reconciler leaves the model admitting new holds.
// Run with the disposable prepayment-wallet-server.ts fixture (its clock is
// advanced a day, like TestRealWalletPrepaymentLifecycle).
func TestNativeRealWalletLostBeginAttemptReplyRefundsWithoutPause(t *testing.T) {
	endpoint := os.Getenv("D34_TEST_WALLET_URL")
	if endpoint == "" {
		t.Skip("requires disposable prepayment-wallet-server.ts")
	}
	require.True(t, strings.HasPrefix(endpoint, "http://127.0.0.1:"))
	ctx := context.Background()
	// A model of its own, so the pause check sees only this test's holds, and
	// a task ID of its own per run, so the test can rerun on one fixture.
	const group, modelID = "u_u", "brand/model/stop-loss-incident"
	id := "native-real-lost-claim-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	operation := "native:" + id

	// edge stands for the platform edge in front of the wallet: it passes
	// every command through, except that it holds the first begin_attempt
	// until the gateway has given up waiting, and only then delivers it.
	var mu sync.Mutex
	var sent []string
	held := false
	lateClaim := make(chan string, 1)
	forward := func(ctx context.Context, r *http.Request, raw []byte) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+r.URL.Path, bytes.NewReader(raw))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", r.Header.Get("Authorization"))
		req.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		return response.StatusCode, body, err
	}
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		var command balance.PrepaymentCommand
		if err := json.Unmarshal(raw, &command); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		action := command.Action
		if command.Outcome != nil {
			action += ":" + command.Outcome.Kind + "/" + command.Outcome.Reason
		}
		mu.Lock()
		sent = append(sent, action)
		hold := command.Action == "begin_attempt" && !held
		held = held || hold
		mu.Unlock()
		if hold {
			// The request context ends when the gateway's client gives up.
			<-r.Context().Done()
			_, body, err := forward(context.Background(), r, raw)
			if err != nil {
				body = []byte(err.Error())
			}
			lateClaim <- string(body)
			return
		}
		status, body, err := forward(r.Context(), r, raw)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	defer edge.Close()
	actions := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), sent...)
	}
	reset := func() {
		mu.Lock()
		defer mu.Unlock()
		sent = nil
	}

	e, plan, _, provider := setup(t)
	e.Wallet = balance.NewExternalHTTP(edge.URL, "local-d34-test-only")
	e.AbandonReservations = true
	plan.Contract = json.RawMessage(strings.Replace(string(plan.Contract), "brand/model/native", modelID, 1))
	requestBody := []byte(strings.Replace(string(body), "brand/model/native", modelID, 1))
	h := &HTTP{Engine: e, Identity: func(*http.Request) (string, int, error) { return group, 1, nil },
		ResolvePlan: func(*http.Request, []byte) (Plan, Provider, error) { return plan, provider, nil },
		limiter:     newReadLimiter(time.Now)}
	submit := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/model-tasks", bytes.NewReader(requestBody))
		r.Header.Set("X-Request-Id", id)
		response := httptest.NewRecorder()
		h.Create(response, r)
		return response
	}
	wallet := balance.NewExternalHTTP(endpoint, "local-d34-test-only")
	walletGet := func() balance.PrepaymentReceipt {
		receipt, err := wallet.Prepayment(ctx, balance.PrepaymentCommand{Action: "get", Group: group, BillingOperationID: operation})
		require.NoError(t, err)
		return receipt
	}
	stored := func() *model.NativeTask {
		task, err := model.GetNativeTask(e.DB, id, group, 1)
		require.NoError(t, err)
		return task
	}
	ledgerNet := func() (int, int64) {
		var rows []struct {
			RequestID string `json:"request_id"`
			Amount    int64  `json:"amount_micros"`
		}
		require.NoError(t, json.Unmarshal(fixturePost(t, endpoint, "/_test/ledger", nil), &rows))
		count, net := 0, int64(0)
		for _, row := range rows {
			if row.RequestID == "prepay:"+operation || row.RequestID == "prepay-refund:"+operation {
				count++
				net += row.Amount
			}
		}
		return count, net
	}

	// The incident: the claim times out on the gateway and lands afterwards.
	started := time.Now()
	response := submit()
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "Retry within a few minutes with the same request ID.")
	require.GreaterOrEqual(t, time.Since(started), 5*time.Second, "the gateway waited its full wallet timeout")
	select {
	case answer := <-lateClaim:
		require.Contains(t, answer, `"claimed":true`, "the wallet recorded the claim after the gateway gave up")
	case <-time.After(30 * time.Second):
		t.Fatal("the held claim never reached the wallet")
	}
	require.Equal(t, []string{"admit", "begin_attempt"}, actions(), "a timed-out claim is not resent")
	require.Zero(t, provider.calls)
	require.Equal(t, "reserved", stored().Status)
	hold := walletGet()
	require.Equal(t, "pending", hold.Status)
	require.Equal(t, "unknown", hold.Submission())
	state, present, err := hold.AttemptState(operation + ":1")
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, "unknown", state)

	// Before the grace recovery only reads the wallet.
	reset()
	report, err := e.RecoverOnce(ctx, "worker", time.Now(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Claimed)
	require.Zero(t, report.Abandoned)
	require.Equal(t, []string{"get"}, actions())

	// After it: failed as not_started first, then released and refunded.
	reset()
	report, err = e.RecoverOnce(ctx, "worker", time.Now().Add(reservationGrace+time.Second), nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Abandoned)
	require.Zero(t, report.Deferred)
	require.Equal(t, []string{"get", "get", "reject_attempt", "settle:failed/not_accepted", "get"}, actions())
	require.Zero(t, provider.calls)
	task := stored()
	require.Equal(t, "failed", task.Status)
	require.Equal(t, model.NotStartedCode, task.ErrorCode)
	require.True(t, task.BillingSettled)

	final := walletGet()
	require.Equal(t, "refunded", final.Status)
	require.Equal(t, "not_submitted", final.Submission(), "the attempt was released before the refund")
	state, present, err = final.AttemptState(operation + ":1")
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, "rejected", state)
	require.NotNil(t, final.ChargedMicros)
	require.NotNil(t, final.RefundMicros)
	require.Zero(t, *final.ChargedMicros)
	require.Equal(t, final.PrepaidMicros, *final.RefundMicros)
	count, net := ledgerNet()
	require.Equal(t, 2, count)
	require.Zero(t, net)

	// The customer's same-ID replay answers the failure and moves no money.
	replay := submit()
	require.Equal(t, http.StatusAccepted, replay.Code, replay.Body.String())
	require.Contains(t, replay.Body.String(), model.NotStartedCode)
	require.Zero(t, provider.calls)
	count, net = ledgerNet()
	require.Equal(t, 2, count)
	require.Zero(t, net)

	// A day later the application's reconciler reviews the row and does not
	// pause the model: a new hold on it is still admitted. (Left pending with
	// an unknown submission, the row would be refunded as submission_timeout
	// and the model paused, as in the incident.)
	fixturePost(t, endpoint, "/_test/advance", map[string]any{"ms": 86400001})
	reviewed := false
	for range 10 {
		var results []struct {
			OperationID string `json:"operationId"`
			State       string `json:"state"`
		}
		require.NoError(t, json.Unmarshal(fixturePost(t, endpoint, "/api/internal/wallet/reconcile", nil), &results))
		for _, result := range results {
			reviewed = reviewed || result.OperationID == operation
		}
		if reviewed || len(results) == 0 {
			break
		}
	}
	require.True(t, reviewed, "the reconciler reviewed the refunded row")
	require.Equal(t, "refunded", walletGet().Status)
	count, net = ledgerNet()
	require.Equal(t, 2, count)
	require.Zero(t, net)
	probe := operation + "-pause-probe"
	prepaid, estimated := int64(200), int64(100)
	_, err = wallet.Prepayment(ctx, balance.PrepaymentCommand{Action: "admit", Group: group, BillingOperationID: probe, TaskID: id + "-pause-probe",
		ModelID: modelID, Currency: "USD", PrepaidMicros: &prepaid, EstimatedMicros: &estimated, QuoteJSON: plan.QuoteJSON})
	require.NoError(t, err, "the model was not paused")
	_, err = wallet.Prepayment(ctx, balance.PrepaymentCommand{Action: "settle", Group: group, BillingOperationID: probe,
		Outcome: &balance.PrepaymentOutcome{Kind: "failed", Reason: "not_accepted"}})
	require.NoError(t, err)
}
