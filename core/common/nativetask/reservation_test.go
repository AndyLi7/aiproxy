package nativetask

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// scriptedWallet fails chosen actions with chosen errors and otherwise behaves like walletStub.
type scriptedWallet struct {
	walletStub
	errs map[string]error
}

func (w *scriptedWallet) Prepayment(ctx context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	if err := w.errs[c.Action]; err != nil {
		w.commands = append(w.commands, c)
		return balance.PrepaymentReceipt{}, err
	}
	return w.walletStub.Prepayment(ctx, c)
}

func TestRefusedAdmissionLeavesNoReservation(t *testing.T) {
	for _, refusal := range []error{balance.ErrPrepaymentInsufficientBalance, balance.ErrPrepaymentTooManyActiveTasks, balance.ErrPrepaymentModelPaused} {
		t.Run(refusal.Error(), func(t *testing.T) {
			e, plan, _, p := setup(t)
			e.Wallet = &scriptedWallet{errs: map[string]error{"admit": refusal}}
			_, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
			require.ErrorIs(t, err, refusal)
			_, err = model.GetNativeTask(e.DB, "req", "g", 1)
			require.ErrorIs(t, err, gorm.ErrRecordNotFound)

			// Once the customer can pay, the same request ID starts a new task.
			e.Wallet = &walletStub{}
			task, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
			require.NoError(t, err)
			require.Equal(t, "queued", task.Status)
			require.Equal(t, 1, p.calls)
		})
	}
}

func TestAmbiguousAdmissionKeepsReservation(t *testing.T) {
	e, plan, _, p := setup(t)
	e.Wallet = &scriptedWallet{errs: map[string]error{"admit": errors.New("prepayment outcome unknown")}}
	_, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.Error(t, err)
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "reserved", task.Status)
	require.Zero(t, p.calls)
}

func TestRetryKeepsReservationFromConcurrentRelease(t *testing.T) {
	e, plan, _, _ := setup(t)
	e.Wallet = &scriptedWallet{errs: map[string]error{"admit": errors.New("prepayment outcome unknown")}}
	_, _ = e.Submit(context.Background(), "req", "g", 1, body, plan)
	first, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	// A retry refreshes the reservation, so a release decided before it is refused.
	_, _ = e.Submit(context.Background(), "req", "g", 1, body, plan)
	released, err := model.ReleaseNativeReservation(e.DB, "req", "g", 1, first.UpdatedAt)
	require.NoError(t, err)
	require.False(t, released)
	retried, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.True(t, retried.UpdatedAt.After(first.UpdatedAt))
	released, err = model.ReleaseNativeReservation(e.DB, "req", "g", 1, retried.UpdatedAt)
	require.NoError(t, err)
	require.True(t, released)
}

func TestRecoveryDeletesOnlyStaleReservationsTheWalletNeverAdmitted(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		getErr  error
		after   time.Duration
		deleted bool
	}{
		{"recent reservation is kept", balance.ErrPrepaymentNotFound, 30 * time.Minute, false},
		{"stale reservation without a wallet record is deleted", balance.ErrPrepaymentNotFound, 2 * time.Hour, true},
		{"stale reservation with an unreadable wallet is kept", errors.New("prepayment unavailable"), 2 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, plan, _, _ := setup(t)
			e.Wallet = &scriptedWallet{errs: map[string]error{"admit": errors.New("prepayment outcome unknown")}}
			_, _ = e.Submit(ctx, "req", "g", 1, body, plan)
			e.Wallet = &scriptedWallet{errs: map[string]error{"get": tc.getErr}}
			report, err := e.RecoverOnce(ctx, "worker", time.Now().Add(tc.after), nil, nil)
			require.NoError(t, err)
			require.Equal(t, 1, report.Claimed)
			_, err = model.GetNativeTask(e.DB, "req", "g", 1)
			if tc.deleted {
				require.ErrorIs(t, err, gorm.ErrRecordNotFound)
				require.Equal(t, 1, report.Advanced)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, report.Deferred)
			}
		})
	}
}

func TestReplayReportsStoredStateWithoutWalletCalls(t *testing.T) {
	e, plan, w, p := setup(t)
	clock := time.Now()
	h := &HTTP{Engine: e, Identity: func(*http.Request) (string, int, error) { return "g", 1, nil }, ResolvePlan: func(*http.Request, []byte) (Plan, Provider, error) { return plan, p, nil },
		limiter: newReadLimiter(func() time.Time { return clock })}
	response := httptest.NewRecorder()
	h.Create(response, request(body))
	require.Equal(t, 202, response.Code)
	calls := len(w.commands)
	for range statusReadBurst {
		response = httptest.NewRecorder()
		h.Create(response, request(body))
		require.Equal(t, 202, response.Code)
	}
	require.Equal(t, calls, len(w.commands), "a replay must not call the wallet")
	response = httptest.NewRecorder()
	h.Create(response, request(body))
	require.Equal(t, 429, response.Code)
	require.Contains(t, response.Body.String(), "rate_limit_exceeded")
	require.Equal(t, 1, p.calls)
}
