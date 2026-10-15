package nativetask

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// appWallet follows the application wallet's prepayment rules that the
// reservation stop-loss relies on (app src/modules/wallet/prepayment.ts):
// admit replays, begin_attempt never claims a known attempt twice and refuses
// a final hold, reject_attempt needs a listed attempt, settle returns a final
// hold unchanged and refuses not_accepted for an accepted submission, and get
// answers the application's own "Prepayment not found" without a hold. Its
// receipts carry submissionStatus, attemptsJson (JSON text), createdAt and
// unknownSince like the application's.
type appWallet struct {
	mu       sync.Mutex
	commands []balance.PrepaymentCommand
	// ctxErrs is each call's context error when it was sent.
	ctxErrs []error

	held         bool
	status       string
	submission   string
	attempts     []walletAttempt
	createdAt    time.Time
	unknownSince *time.Time

	// clock stamps createdAt and unknownSince; time.Now when nil.
	clock func() time.Time
	// lose commits these actions once, then reports an unknown outcome.
	lose map[string]bool
	// down refuses these actions before they reach the wallet.
	down map[string]bool
	// attemptsJSON replaces the attempt list in receipts when set.
	attemptsJSON json.RawMessage
	// onBegin, when set, runs during begin_attempt: before its commit when
	// beforeCommit is set, else after it and before the reply.
	onBegin      func()
	beforeCommit bool
	// notFound answers any command but admit without a hold; the
	// application's own "Prepayment not found" when nil.
	notFound error
	// beforeRefund, when set, runs as reject_attempt or settle arrives,
	// before anything else; refundChecks counts its runs.
	beforeRefund func(balance.PrepaymentCommand)
	refundChecks int
}

type walletAttempt struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

var errWalletLost = errors.New("prepayment outcome unknown; retry original billing operation")

func (w *appWallet) now() time.Time {
	if w.clock != nil {
		return w.clock()
	}
	return time.Now()
}

func (w *appWallet) Prepayment(ctx context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	if c.Action == "begin_attempt" && w.onBegin != nil && w.beforeCommit {
		w.onBegin()
	}
	if (c.Action == "reject_attempt" || c.Action == "settle") && w.beforeRefund != nil {
		w.beforeRefund(c)
		w.mu.Lock()
		w.refundChecks++
		w.mu.Unlock()
	}
	w.mu.Lock()
	w.commands = append(w.commands, c)
	w.ctxErrs = append(w.ctxErrs, ctx.Err())
	if w.down[c.Action] {
		w.mu.Unlock()
		return balance.PrepaymentReceipt{}, errWalletLost
	}
	receipt, err := w.apply(c)
	lost := w.lose[c.Action]
	delete(w.lose, c.Action)
	w.mu.Unlock()
	if c.Action == "begin_attempt" && w.onBegin != nil && !w.beforeCommit {
		w.onBegin()
	}
	if lost && err == nil {
		return balance.PrepaymentReceipt{}, errWalletLost
	}
	return receipt, err
}

func (w *appWallet) apply(c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	if c.Action == "admit" && !w.held {
		w.held, w.status, w.submission, w.createdAt = true, "pending", "not_submitted", w.now()
	}
	if !w.held {
		return balance.PrepaymentReceipt{}, w.notFound
	}
	final := w.status != "pending"
	find := func(id string) *walletAttempt {
		for i := range w.attempts {
			if w.attempts[i].ID == id {
				return &w.attempts[i]
			}
		}
		return nil
	}
	switch c.Action {
	case "begin_attempt":
		if final {
			return balance.PrepaymentReceipt{}, errors.New("prepayment rejected or unavailable")
		}
		if find(c.AttemptID) != nil {
			return balance.PrepaymentReceipt{ID: c.BillingOperationID, Claimed: false}, nil
		}
		at := w.now()
		w.attempts = append(w.attempts, walletAttempt{ID: c.AttemptID, State: "unknown"})
		w.submission, w.unknownSince = "unknown", &at
		return balance.PrepaymentReceipt{ID: c.BillingOperationID, Claimed: true}, nil
	case "reject_attempt", "accept_attempt":
		attempt := find(c.AttemptID)
		if attempt == nil {
			return balance.PrepaymentReceipt{}, errors.New("prepayment rejected or unavailable")
		}
		state := "rejected"
		if c.Action == "accept_attempt" {
			state = "accepted"
		}
		if attempt.State != "unknown" && attempt.State != state {
			return balance.PrepaymentReceipt{}, errors.New("prepayment rejected or unavailable")
		}
		attempt.State = state
		if !final {
			w.submission, w.unknownSince = map[string]string{"rejected": "not_submitted", "accepted": "accepted"}[state], nil
		}
	case "settle":
		if !final {
			if c.Outcome.Reason == "not_accepted" && w.submission == "accepted" {
				return balance.PrepaymentReceipt{}, errors.New("prepayment rejected or unavailable")
			}
			if c.Outcome.Reason == "not_dispatched" && len(w.attempts) > 0 {
				return balance.PrepaymentReceipt{}, balance.ErrPrepaymentRejected
			}
			w.status = "refunded"
		}
	}
	return w.receipt(c.BillingOperationID), nil
}

func rawJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func (w *appWallet) receipt(id string) balance.PrepaymentReceipt {
	attempts := w.attemptsJSON
	if attempts == nil {
		list := w.attempts
		if list == nil {
			list = []walletAttempt{}
		}
		attempts = rawJSON(string(rawJSON(list)))
	}
	unknownSince := json.RawMessage("null")
	if w.unknownSince != nil {
		unknownSince = rawJSON(w.unknownSince.UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	r := balance.PrepaymentReceipt{
		ID: id, Status: w.status, PrepaidMicros: 200,
		SubmissionStatus: rawJSON(w.submission), AttemptsJSON: attempts,
		CreatedAt: rawJSON(w.createdAt.UTC().Format("2006-01-02T15:04:05.000Z")), UnknownSince: unknownSince,
	}
	if w.status == "refunded" {
		zero, full := int64(0), int64(200)
		r.ChargedMicros, r.RefundMicros = &zero, &full
	}
	return r
}

// actions lists wallet calls since the last reset, settle outcomes spelled out.
func (w *appWallet) actions() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return walletActions(&walletStub{commands: w.commands})
}

func (w *appWallet) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.commands, w.ctxErrs = nil, nil
}

func (w *appWallet) state() (status, submission string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status, w.submission
}

// Bodies of a wallet 404: the application's own answer, which confirms there
// is no hold, and a platform page (for example during a deploy), which does not.
const (
	appNotFound  = `{"code":-1,"message":"Prepayment not found"}`
	edgeNotFound = `<html><body>Not Found</body></html>`
)

// walletNotFound is the error the wallet client returns for a 404 with body.
func walletNotFound(t *testing.T, body string) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	_, err := balance.NewExternalHTTP(server.URL, "test-only").Prepayment(context.Background(), balance.PrepaymentCommand{Action: "get", Group: "g", BillingOperationID: "native:req"})
	require.ErrorIs(t, err, balance.ErrPrepaymentNotFound)
	return err
}

// stopLoss is an engine with the stop-loss on, an application-like wallet and
// an HTTP handler, the way production wires them. The wallet checks that a
// refund command only ever reaches it for a row already failed as not_started:
// the row is locked before any wallet write.
func stopLoss(t *testing.T) (*Engine, *appWallet, *providerStub, *HTTP) {
	t.Helper()
	e, plan, _, p := setup(t)
	w := &appWallet{lose: map[string]bool{}, down: map[string]bool{}, notFound: walletNotFound(t, appNotFound)}
	w.beforeRefund = func(c balance.PrepaymentCommand) {
		task, err := model.GetNativeTask(e.DB, "req", "g", 1)
		if err != nil {
			t.Errorf("%s: read the task: %v", c.Action, err)
		} else if task.Status != "failed" || task.ErrorCode != model.NotStartedCode {
			// t.Errorf, not require: the call may come from a request goroutine.
			t.Errorf("%s reached the wallet before the row was failed as not_started (status %q, error_code %q)", c.Action, task.Status, task.ErrorCode)
		}
	}
	e.Wallet = w
	e.AbandonReservations = true
	h := &HTTP{Engine: e, Identity: func(*http.Request) (string, int, error) { return "g", 1, nil },
		ResolvePlan: func(*http.Request, []byte) (Plan, Provider, error) { return plan, p, nil },
		limiter:     newReadLimiter(time.Now)}
	return e, w, p, h
}

func create(h *HTTP) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	h.Create(response, request(body))
	return response
}

type publicTask struct {
	Status string `json:"status"`
	Error  struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func readPublic(t *testing.T, response *httptest.ResponseRecorder) publicTask {
	t.Helper()
	var public publicTask
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &public), response.Body.String())
	return public
}

func storedTask(t *testing.T, e *Engine) *model.NativeTask {
	t.Helper()
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	return task
}

// requireNotStarted checks a task recovery failed as not_started and refunded,
// and what its customer sees: GET and a same-ID replay both answer failed,
// not charged, with a new X-Request-Id asked for.
func requireNotStarted(t *testing.T, e *Engine, h *HTTP) {
	t.Helper()
	task := storedTask(t, e)
	require.Equal(t, "failed", task.Status)
	require.Equal(t, model.NotStartedCode, task.ErrorCode)
	require.True(t, task.BillingSettled)
	get := httptest.NewRecorder()
	h.Get(get, httptest.NewRequest(http.MethodGet, "/v1/model-tasks/req", nil), "req")
	replay := create(h)
	for status, response := range map[int]*httptest.ResponseRecorder{200: get, 202: replay} {
		require.Equal(t, status, response.Code, response.Body.String())
		public := readPublic(t, response)
		require.Equal(t, "failed", public.Status)
		require.Equal(t, model.NotStartedCode, public.Error.Code)
		require.Equal(t, "This task was not started because of a temporary platform error. You were not charged; submit a new task with a new X-Request-Id.", public.Error.Message)
	}
}

// Incident 2026-10-13: the wallet processed begin_attempt after the gateway's
// timeout, so the gateway saw an unknown outcome, never called the provider
// and left the task reserved. It used to stay held for 24 hours and then
// pause the model. Now, once both sides have been quiet for the grace, it is
// failed as not_started and refunded as not_accepted: the attempt is
// released, then the hold settled, and the provider is never called.
func TestLostBeginAttemptReplyRefundsWithoutProviderCall(t *testing.T) {
	ctx := context.Background()
	e, w, p, h := stopLoss(t)
	w.lose["begin_attempt"] = true
	response := create(h)
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "Retry within a few minutes with the same request ID.")
	require.Equal(t, "reserved", storedTask(t, e).Status)
	_, submission := w.state()
	require.Equal(t, "unknown", submission)
	w.reset()
	start := time.Now()

	report, err := e.RecoverOnce(ctx, "worker", start.Add(reservationGrace-time.Second), nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Claimed)
	require.Zero(t, report.Abandoned)
	require.Equal(t, []string{"get"}, w.actions(), "no wallet write before the grace")
	require.Equal(t, "reserved", storedTask(t, e).Status)
	w.reset()

	report, err = e.RecoverOnce(ctx, "worker", start.Add(reservationGrace+time.Second), nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Abandoned)
	require.Equal(t, []string{"get", "get", "reject_attempt", "settle:failed/not_accepted", "get"}, w.actions())
	require.Equal(t, "native:req:1", w.commands[2].AttemptID)
	require.Equal(t, 2, w.refundChecks, "the release and the settle each found the row already failed")
	status, submission := w.state()
	require.Equal(t, "refunded", status)
	require.Equal(t, "not_submitted", submission, "the attempt is released, so the application retires the hold without a pause")
	require.Zero(t, p.calls)
	requireNotStarted(t, e, h)

	// A settled failure is not claimed again.
	report, err = e.RecoverOnce(ctx, "worker", start.Add(time.Hour), nil, nil)
	require.NoError(t, err)
	require.Zero(t, report.Claimed)
	require.Zero(t, p.calls)
}

// The admission was held but its reply was lost: there is no attempt to
// release, so only the settle is sent (reject_attempt on a missing attempt is
// refused for good). With no attempt it is not_dispatched (own task queue
// spec §4.3), which the wallet accepts only while no attempt was claimed.
func TestLostAdmitReplyRefundsWithoutReject(t *testing.T) {
	e, w, p, h := stopLoss(t)
	w.lose["admit"] = true
	require.Equal(t, http.StatusServiceUnavailable, create(h).Code)
	w.reset()
	report, err := e.RecoverOnce(context.Background(), "worker", time.Now().Add(reservationGrace+time.Second), nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Abandoned)
	require.Equal(t, []string{"get", "get", "settle:failed/not_dispatched", "get"}, w.actions())
	status, submission := w.state()
	require.Equal(t, "refunded", status)
	require.Equal(t, "not_submitted", submission)
	require.Zero(t, p.calls)
	requireNotStarted(t, e, h)
}

// A same-ID retry still in flight when recovery abandons the reservation
// never reaches the provider: recovery's compare-and-set comes first, so the
// retry's own reserved->submitting transition matches no row. The retry
// answers 202 with the stored failure, not 409 request_id_conflict.
func TestAbandonFencesInFlightSubmit(t *testing.T) {
	for _, tc := range []struct {
		name         string
		beforeCommit bool
		actions      []string
	}{
		// The claim landed before recovery read the wallet: it is released.
		{"claim landed first", false, []string{"get", "get", "reject_attempt", "settle:failed/not_accepted", "get"}},
		// The claim reaches the wallet after the refund and is refused.
		{"claim landed after the refund", true, []string{"get", "get", "settle:failed/not_dispatched", "get"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, w, p, h := stopLoss(t)
			// The reservation and its hold are ten minutes old (the first
			// admission's reply was lost); a same-ID retry is in flight.
			old := func() time.Time { return time.Now().Add(-10 * time.Minute) }
			w.clock = old
			w.lose["admit"] = true
			require.Equal(t, http.StatusServiceUnavailable, create(h).Code)
			require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "req").Update("created_at", old()).Error)

			entered, release := make(chan struct{}), make(chan struct{})
			w.beforeCommit = tc.beforeCommit
			w.onBegin = func() {
				close(entered)
				<-release
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- create(h) }()
			<-entered
			w.reset()

			report, err := e.RecoverOnce(context.Background(), "worker", time.Now(), nil, nil)
			require.NoError(t, err)
			require.Equal(t, 1, report.Abandoned)
			close(release)
			response := <-done

			require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
			public := readPublic(t, response)
			require.Equal(t, "failed", public.Status)
			require.Equal(t, model.NotStartedCode, public.Error.Code)
			require.Zero(t, p.calls, "the retry never reaches the provider")
			actions := w.actions()
			if tc.beforeCommit {
				// The refused claim is the retry's own call, after the refund.
				require.Equal(t, append(tc.actions, "begin_attempt"), actions)
			} else {
				require.Equal(t, tc.actions, actions)
			}
			status, _ := w.state()
			require.Equal(t, "refunded", status)
		})
	}
}

// Without a hold (the admission never reached the wallet) there is nothing to
// refund: the reservation is not abandoned, and it is deleted after an hour
// only when the application itself answers "Prepayment not found". A bare 404
// (a platform page during a deploy) proves nothing about the hold, so it never
// deletes a reservation the stop-loss may still have to refund.
func TestReservationWithoutHoldIsNotAbandoned(t *testing.T) {
	ctx := context.Background()
	e, w, _, h := stopLoss(t)
	w.down["admit"] = true
	require.Equal(t, http.StatusServiceUnavailable, create(h).Code)
	w.reset()
	now := time.Now()
	report, err := e.RecoverOnce(ctx, "worker", now.Add(reservationGrace+time.Second), nil, nil)
	require.NoError(t, err)
	require.Zero(t, report.Abandoned)
	require.Equal(t, []string{"get"}, w.actions())
	require.Equal(t, "reserved", storedTask(t, e).Status)

	w.notFound = walletNotFound(t, edgeNotFound)
	report, err = e.RecoverOnce(ctx, "worker", now.Add(2*time.Hour), nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Claimed)
	require.Equal(t, 1, report.Deferred)
	require.Equal(t, "reserved", storedTask(t, e).Status, "a bare 404 never deletes a reservation")

	w.notFound = walletNotFound(t, appNotFound)
	report, err = e.RecoverOnce(ctx, "worker", now.Add(3*time.Hour), nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Claimed)
	require.Equal(t, 1, report.Advanced)
	_, err = model.GetNativeTask(e.DB, "req", "g", 1)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

// While the wallet cannot be read nothing is decided; the next pass refunds.
func TestReservationNotAbandonedWhileWalletUnavailable(t *testing.T) {
	ctx := context.Background()
	e, w, p, h := stopLoss(t)
	w.lose["begin_attempt"] = true
	require.Equal(t, http.StatusServiceUnavailable, create(h).Code)
	w.reset()
	w.down["get"] = true
	now := time.Now()
	report, err := e.RecoverOnce(ctx, "worker", now.Add(reservationGrace+time.Second), nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Deferred)
	require.Zero(t, report.Abandoned)
	require.Equal(t, "reserved", storedTask(t, e).Status)
	require.Equal(t, []string{"get"}, w.actions())

	delete(w.down, "get")
	report, err = e.RecoverOnce(ctx, "worker", now.Add(time.Hour), nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Abandoned)
	require.Zero(t, p.calls)
	requireNotStarted(t, e, h)
}

// A wallet that recorded a provider acceptance this gateway never saw is left
// for an operator: it must never be refunded as not accepted.
func TestReservationWithAcceptedWalletAttemptIsLeftAlone(t *testing.T) {
	e, w, _, h := stopLoss(t)
	w.lose["begin_attempt"] = true
	require.Equal(t, http.StatusServiceUnavailable, create(h).Code)
	w.mu.Lock()
	w.attempts[0].State, w.submission = "accepted", "accepted"
	w.mu.Unlock()
	w.reset()
	report, err := e.RecoverOnce(context.Background(), "worker", time.Now().Add(time.Hour), nil, nil)
	require.NoError(t, err)
	require.Zero(t, report.Abandoned)
	require.Equal(t, []string{"get"}, w.actions())
	require.Equal(t, "reserved", storedTask(t, e).Status)
}

// An attempt list the gateway cannot read still ends in a refund: the
// release is skipped, the settle is sent (valid from an unknown submission).
func TestUnreadableWalletAttemptsStillRefund(t *testing.T) {
	for _, attempts := range []json.RawMessage{rawJSON("not json"), rawJSON(map[string]int{"x": 1}), json.RawMessage("null")} {
		t.Run(string(attempts), func(t *testing.T) {
			e, w, p, h := stopLoss(t)
			w.lose["begin_attempt"] = true
			require.Equal(t, http.StatusServiceUnavailable, create(h).Code)
			w.attemptsJSON = attempts
			w.reset()
			report, err := e.RecoverOnce(context.Background(), "worker", time.Now().Add(reservationGrace+time.Second), nil, nil)
			require.NoError(t, err)
			require.Equal(t, 1, report.Abandoned)
			require.Equal(t, []string{"get", "get", "settle:failed/not_accepted", "get"}, w.actions())
			status, _ := w.state()
			require.Equal(t, "refunded", status)
			require.Zero(t, p.calls)
			requireNotStarted(t, e, h)
		})
	}
}

// A refund that fails after the row was failed is retried by later passes,
// also once the stop-loss is switched off (the rollback procedure waits for
// these rows to settle).
func TestNotStartedRefundRetriesUntilSettled(t *testing.T) {
	ctx := context.Background()
	e, w, p, h := stopLoss(t)
	w.lose["begin_attempt"] = true
	require.Equal(t, http.StatusServiceUnavailable, create(h).Code)
	w.down["settle"] = true
	now := time.Now()
	report, err := e.RecoverOnce(ctx, "worker", now.Add(reservationGrace+time.Second), nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Abandoned)
	require.Equal(t, 1, report.Deferred)
	task := storedTask(t, e)
	require.Equal(t, "failed", task.Status)
	require.Equal(t, model.NotStartedCode, task.ErrorCode)
	require.False(t, task.BillingSettled)

	delete(w.down, "settle")
	e.AbandonReservations = false
	report, err = e.RecoverOnce(ctx, "worker", now.Add(time.Hour), nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Advanced)
	status, _ := w.state()
	require.Equal(t, "refunded", status)
	require.Zero(t, p.calls)
	requireNotStarted(t, e, h)
}

// The rollback switch: with the stop-loss off, a reservation keeps waiting.
func TestStopLossOffKeepsReservations(t *testing.T) {
	e, w, _, h := stopLoss(t)
	e.AbandonReservations = false
	w.lose["begin_attempt"] = true
	require.Equal(t, http.StatusServiceUnavailable, create(h).Code)
	w.reset()
	report, err := e.RecoverOnce(context.Background(), "worker", time.Now().Add(time.Hour), nil, nil)
	require.NoError(t, err)
	require.Zero(t, report.Abandoned)
	require.Equal(t, []string{"get"}, w.actions())
	require.Equal(t, "reserved", storedTask(t, e).Status)
}

// A reservation is checked again right when it becomes quiet instead of up to
// the backoff later: one second past five minutes after the reservation, the
// admission and the claim, which here all happened at the same second.
func TestNativeRecoveryRechecksReservationAtGrace(t *testing.T) {
	e, w, _, h := stopLoss(t)
	reserved := time.Now().Truncate(time.Second)
	w.clock = func() time.Time { return reserved }
	w.lose["begin_attempt"] = true
	require.Equal(t, http.StatusServiceUnavailable, create(h).Code)
	require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "req").Update("created_at", reserved).Error)
	_, err := e.RecoverOnce(context.Background(), "worker", reserved.Add(4*time.Minute+30*time.Second), nil, nil)
	require.NoError(t, err)
	task := storedTask(t, e)
	require.Equal(t, "reserved", task.Status)
	require.Equal(t, reserved.Add(5*time.Minute+time.Second).Unix(), task.NextRecoveryAt)
}

// The wallet's own times keep a reservation alive (S1 quiet check): the
// gateway row is ten minutes old, but a same-ID retry admitted and claimed it
// one minute ago. It is not abandoned until five minutes after the wallet's
// last time, and recovery looks at it again exactly then.
func TestReservationWaitsUntilTheWalletIsQuiet(t *testing.T) {
	ctx := context.Background()
	e, w, p, h := stopLoss(t)
	now := time.Now().Truncate(time.Second)
	walletAt := now.Add(-time.Minute)
	w.clock = func() time.Time { return walletAt }
	w.lose["begin_attempt"] = true
	require.Equal(t, http.StatusServiceUnavailable, create(h).Code)
	require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "req").Update("created_at", now.Add(-10*time.Minute)).Error)
	wake := walletAt.Add(5*time.Minute + time.Second)
	w.reset()

	report, err := e.RecoverOnce(ctx, "worker", now, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Claimed)
	require.Zero(t, report.Abandoned)
	require.Equal(t, []string{"get"}, w.actions())
	task := storedTask(t, e)
	require.Equal(t, "reserved", task.Status)
	require.Equal(t, wake.UTC(), reservationWake(task).UTC())
	require.LessOrEqual(t, task.NextRecoveryAt, wake.Unix(), "never checked later than when the wallet is quiet")

	// One second before the wallet is quiet it is still kept, and the next
	// look is the wake itself (the age backoff would be later).
	w.reset()
	report, err = e.RecoverOnce(ctx, "worker", walletAt.Add(5*time.Minute-time.Second), nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Claimed)
	require.Zero(t, report.Abandoned)
	require.Equal(t, []string{"get"}, w.actions())
	task = storedTask(t, e)
	require.Equal(t, "reserved", task.Status)
	require.Equal(t, wake.Unix(), task.NextRecoveryAt)

	report, err = e.RecoverOnce(ctx, "worker", wake, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Abandoned)
	require.Zero(t, p.calls)
	requireNotStarted(t, e, h)
}

// A same-ID retry that just admitted or claimed keeps the reservation alive:
// the wallet's own times must be quiet too, not only the gateway's.
func TestReservationQuietNeedsTheWalletQuietToo(t *testing.T) {
	reserved := time.Date(2026, 10, 13, 11, 0, 0, 0, time.UTC)
	task := &model.NativeTask{CreatedAt: reserved}
	hold := func(submission, admitted string, unknown any) balance.PrepaymentReceipt {
		return balance.PrepaymentReceipt{Status: "pending", SubmissionStatus: rawJSON(submission), CreatedAt: rawJSON(admitted), UnknownSince: rawJSON(unknown)}
	}
	quietAt, ok := reservationQuietAt(task, hold("not_submitted", "2026-10-13T11:50:00.000Z", nil))
	require.True(t, ok)
	require.Equal(t, reserved.Add(50*time.Minute+reservationGrace), quietAt)
	quietAt, ok = reservationQuietAt(task, hold("unknown", "2026-10-13T11:00:01.000Z", "2026-10-13T11:20:00.000Z"))
	require.True(t, ok)
	require.Equal(t, reserved.Add(20*time.Minute+reservationGrace), quietAt)
	_, ok = reservationQuietAt(task, hold("unknown", "2026-10-13T11:00:01.000Z", nil))
	require.False(t, ok, "an unknown attempt without its claim time is never assumed quiet")
	_, ok = reservationQuietAt(task, balance.PrepaymentReceipt{Status: "pending", SubmissionStatus: rawJSON("not_submitted"), CreatedAt: rawJSON(12)})
	require.False(t, ok)
}

// R3: a client that disconnects mid-submission no longer cancels the wallet
// calls or the provider submission; the task is submitted once.
func TestSubmitWalletCallsSurviveClientDisconnect(t *testing.T) {
	_, w, p, h := stopLoss(t)
	ctx, disconnect := context.WithCancel(context.Background())
	provider := &contextProvider{providerStub: p}
	resolve := h.ResolvePlan
	h.ResolvePlan = func(r *http.Request, raw []byte) (Plan, Provider, error) {
		plan, _, err := resolve(r, raw)
		return plan, provider, err
	}
	// The client goes away after the admission, before the claim is sent.
	w.onBegin, w.beforeCommit = disconnect, true
	response := httptest.NewRecorder()
	h.Create(response, request(body).WithContext(ctx))
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	require.Equal(t, "queued", readPublic(t, response).Status)
	require.Equal(t, []string{"admit", "begin_attempt", "accept_attempt"}, w.actions())
	require.Equal(t, []error{nil, nil, nil}, w.ctxErrs)
	require.Equal(t, 1, p.calls)
	require.NoError(t, provider.ctxErr)
	require.Error(t, ctx.Err(), "the client did disconnect")
}

// The admission and the claim share one wallet budget. When admission left
// the claim less than its full request timeout, the claim is not sent: a
// claim cut short that the wallet records after the gateway gave up is the
// 2026-10-13 incident. The task stays reserved with a not_submitted hold, so
// the same-ID retry the 503 asks for admits (a replay) and claims normally.
func TestNativeClaimNeedsTheFullWalletRequestTimeout(t *testing.T) {
	e, w, p, h := stopLoss(t)
	budget := submitWalletBudget
	t.Cleanup(func() { submitWalletBudget = budget })
	submitWalletBudget = 4 * time.Second

	response := create(h)
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "Retry within a few minutes with the same request ID.")
	require.Equal(t, []string{"admit"}, w.actions(), "no claim is sent")
	require.Equal(t, "reserved", storedTask(t, e).Status)
	status, submission := w.state()
	require.Equal(t, "pending", status)
	require.Equal(t, "not_submitted", submission)
	require.Zero(t, p.calls)

	submitWalletBudget = budget
	w.reset()
	response = create(h)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	require.Equal(t, "queued", readPublic(t, response).Status)
	require.Equal(t, []string{"admit", "begin_attempt", "accept_attempt"}, w.actions())
	require.Equal(t, 1, p.calls)
}

// contextProvider records the context error its submission saw.
type contextProvider struct {
	*providerStub
	ctxErr error
}

func (p *contextProvider) SubmitNative(ctx context.Context, endpoint string, b []byte) (string, error) {
	p.ctxErr = ctx.Err()
	return p.providerStub.SubmitNative(ctx, endpoint, b)
}

// Owner decision 2026-10-13 orders the timings: the grace exceeds a whole
// submission, which exceeds its wallet budget, so recovery never abandons a
// reservation whose own submission may still be running.
func TestStopLossTimingOrder(t *testing.T) {
	require.Equal(t, 5*time.Minute, reservationGrace)
	require.Greater(t, reservationGrace, submitTimeout)
	require.Greater(t, submitTimeout, submitWalletBudget)
	require.LessOrEqual(t, submitWalletBudget, 15*time.Second)
	require.Less(t, reservationRefundBudget, 2*time.Minute, "inside the recovery lease")
}
