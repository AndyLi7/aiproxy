package nativetask

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"testing"
)

type walletStub struct {
	commands []balance.PrepaymentCommand
	claimed  bool
	fail     string
}

func (w *walletStub) Prepayment(_ context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	w.commands = append(w.commands, c)
	if c.Action == w.fail {
		return balance.PrepaymentReceipt{}, errors.New("timeout")
	}
	r := balance.PrepaymentReceipt{ID: c.BillingOperationID}
	if c.Action == "begin_attempt" {
		r.Claimed = !w.claimed
		w.claimed = true
	}
	return r, nil
}

type providerStub struct {
	calls int
	body  []byte
	err   error
}

func (p *providerStub) SubmitNative(_ context.Context, _ string, b []byte) (string, error) {
	p.calls++
	p.body = append([]byte(nil), b...)
	return "upstream-1", p.err
}
func setup(t *testing.T) (*Engine, Plan, *walletStub, *providerStub) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "native.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	w := &walletStub{}
	p := &providerStub{}
	engine := &Engine{DB: db, Wallet: w, Provider: p}
	plan := Plan{KeyFingerprint: model.ImageChannelKeyFingerprint("test-native-key"), Contract: json.RawMessage(`{"version":1,"model":"brand/model/native","input_schema":{"type":"object","required":["seed"],"properties":{"seed":{"type":"integer"}},"additionalProperties":false},"output_schema":{"type":"object"}}`), ChannelID: 1, Endpoint: "fal-ai/vector/model", CredentialScope: "scope", QuoteJSON: `{"version":1,"currency":"USD","quoteVersion":"verified","prepaidMicros":200,"routes":[{"routeId":"r","channelId":1,"provider":"fal","endpoint":"fal-ai/vector/model","credentialScope":"scope","quantityMetric":"bounded_request","estimatedMicros":100,"prepaidMicros":200,"rule":{"mode":"cost_markup","ratio":"1"}}]}`}
	return engine, plan, w, p
}

var body = []byte(`{"model":"brand/model/native","input":{"seed":9007199254740993}}`)

func TestNativeExecutionReserveBeforeSubmitAndReplay(t *testing.T) {
	e, plan, w, p := setup(t)
	task, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.Equal(t, "queued", task.Status)
	require.Equal(t, []string{"admit", "begin_attempt", "accept_attempt"}, []string{w.commands[0].Action, w.commands[1].Action, w.commands[2].Action})
	require.Equal(t, 1, p.calls)
	require.Equal(t, `{"seed":9007199254740993}`, string(p.body))
	_, err = e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.Equal(t, 1, p.calls)
	_, err = e.Submit(context.Background(), "req", "g", 2, body, plan)
	require.ErrorIs(t, err, model.ErrNativeTaskConflict)
	require.Equal(t, 1, p.calls)
	for _, c := range w.commands {
		require.Equal(t, "g", c.Group)
		require.Equal(t, "native:req", c.BillingOperationID)
	}
}
func TestNativeExecutionWalletUnknownNeverSubmits(t *testing.T) {
	for _, action := range []string{"admit", "begin_attempt"} {
		t.Run(action, func(t *testing.T) {
			e, plan, w, p := setup(t)
			w.fail = action
			_, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
			require.Error(t, err)
			require.Zero(t, p.calls)
		})
	}
	e, plan, w, p := setup(t)
	w.claimed = true
	_, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.ErrorIs(t, err, ErrPending)
	require.Zero(t, p.calls)
}
func TestNativeExecutionUnknownProviderOutcomeNeverResubmits(t *testing.T) {
	e, plan, w, p := setup(t)
	p.err = errors.New("connection lost after write")
	task, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.ErrorIs(t, err, ErrPending)
	require.Equal(t, "submission_unknown", task.Status)
	p.err = nil
	_, err = e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.Equal(t, 1, p.calls)
	require.Equal(t, "get", w.commands[len(w.commands)-1].Action)
}
func TestNativeExecutionRejectedSubmissionReleasesViaWallet(t *testing.T) {
	e, plan, w, p := setup(t)
	p.err = adaptor.ErrImageSubmissionRejected
	task, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.Equal(t, "failed", task.Status)
	require.Equal(t, "reject_attempt", w.commands[2].Action)
	require.Equal(t, "settle", w.commands[3].Action)
	require.Equal(t, "failed", w.commands[3].Outcome.Kind)
	require.Equal(t, "not_accepted", w.commands[3].Outcome.Reason)
}
func TestNativeExecutionInvalidBindingNeverReservesFunds(t *testing.T) {
	e, plan, w, p := setup(t)
	plan.CredentialScope = "different"
	_, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.ErrorIs(t, err, ErrUnavailable)
	require.Empty(t, w.commands)
	require.Zero(t, p.calls)
}

func TestNativeExecutionRecoversAcceptedWalletCallback(t *testing.T) {
	e, plan, w, p := setup(t)
	w.fail = "accept_attempt"
	task, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.Error(t, err)
	require.Equal(t, "queued", task.Status)
	w.fail = ""
	task, err = e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.Equal(t, 1, p.calls)
	require.NotEmpty(t, task.BillingReceiptJSON)
}
func TestNativeExecutionRecoversRejectedWalletCallback(t *testing.T) {
	e, plan, w, p := setup(t)
	p.err = adaptor.ErrImageSubmissionRejected
	w.fail = "reject_attempt"
	task, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.Error(t, err)
	require.Equal(t, "failed", task.Status)
	w.fail = ""
	_, err = e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.Equal(t, 1, p.calls)
	require.Equal(t, "settle", w.commands[len(w.commands)-2].Action)
}

func TestNativeControlsValidatedBeforeWalletOrProvider(t *testing.T) {
	e, plan, w, p := setup(t)
	plan.Contract = json.RawMessage(`{"version":1,"model":"brand/model/native","input_schema":{"type":"object","required":["seed"],"properties":{"seed":{"type":"integer"}},"additionalProperties":true},"upstream_input_schema":{"type":"object","properties":{"seed":{"type":"integer"},"enable_safety_checker":{"const":true}},"required":["seed","enable_safety_checker"],"additionalProperties":false},"fixed_parameters":{"enable_safety_checker":true},"output_schema":{}}`)
	_, err := e.Submit(context.Background(), "bad", "g", 1, []byte(`{"model":"brand/model/native","input":{"seed":1,"enable_safety_checker":false}}`), plan)
	require.Error(t, err)
	require.Empty(t, w.commands)
	require.Zero(t, p.calls)
	_, err = e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.Equal(t, `{"enable_safety_checker":true,"seed":9007199254740993}`, string(p.body))
}
