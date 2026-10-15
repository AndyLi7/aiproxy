package nativetask

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// queueHold is one hold in queueWallet.
type queueHold struct {
	group        string
	status       string
	submission   string
	attempts     []walletAttempt
	createdAt    time.Time
	unknownSince *time.Time
}

// queueWallet follows the application wallet's rules for the own task queue
// (app src/modules/wallet/prepayment.ts, spec 2026-10-15 §4): a queued admit
// stores the hold waiting and checks the waiting limit; begin_attempt on a
// waiting hold checks a negative balance (402), a paused model (423) and the
// active limit without waiting holds or itself (429 with activeLimit), and
// leaves the hold unchanged when it refuses; settle not_dispatched is refused
// once an attempt was claimed. Every hold is 200 micros.
type queueWallet struct {
	mu          sync.Mutex
	holds       map[string]*queueHold
	balances    map[string]int64
	activeLimit int
	maxWaiting  int
	paused      bool
	commands    []balance.PrepaymentCommand
	// beforeBegin runs as begin_attempt arrives, before it is applied;
	// afterBegin after it was applied, before the reply.
	beforeBegin func(balance.PrepaymentCommand)
	afterBegin  func(balance.PrepaymentCommand)
	// down refuses these actions with an unknown outcome, before applying.
	down map[string]bool
	// lose applies these actions once, then reports an unknown outcome.
	lose map[string]bool
}

func newQueueWallet() *queueWallet {
	return &queueWallet{holds: map[string]*queueHold{}, balances: map[string]int64{}, maxWaiting: 20,
		down: map[string]bool{}, lose: map[string]bool{}}
}

func (w *queueWallet) Prepayment(_ context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	if c.Action == "begin_attempt" && w.beforeBegin != nil {
		w.beforeBegin(c)
	}
	w.mu.Lock()
	w.commands = append(w.commands, c)
	if w.down[c.Action] {
		w.mu.Unlock()
		return balance.PrepaymentReceipt{}, errWalletLost
	}
	receipt, err := w.apply(c)
	lost := err == nil && w.lose[c.Action]
	delete(w.lose, c.Action)
	w.mu.Unlock()
	if c.Action == "begin_attempt" && w.afterBegin != nil {
		w.afterBegin(c)
	}
	if lost {
		return balance.PrepaymentReceipt{}, errWalletLost
	}
	return receipt, err
}

func (w *queueWallet) balance(group string) int64 {
	if value, ok := w.balances[group]; ok {
		return value
	}
	return 1000
}

// active counts the group's unsettled holds that count toward the active limit.
func (w *queueWallet) active(group, except string) int {
	count := 0
	for id, hold := range w.holds {
		if id != except && hold.group == group && hold.status == "pending" && hold.submission != "waiting" {
			count++
		}
	}
	return count
}

func (w *queueWallet) apply(c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	hold := w.holds[c.BillingOperationID]
	if c.Action == "admit" {
		if hold != nil {
			return w.receipt(c.BillingOperationID), nil
		}
		if c.Queue {
			waiting := 0
			for _, other := range w.holds {
				if other.group == c.Group && other.status == "pending" && other.submission == "waiting" {
					waiting++
				}
			}
			if waiting >= w.maxWaiting {
				return balance.PrepaymentReceipt{}, &balance.TaskLimitError{Waiting: true, ActiveLimit: w.activeLimit}
			}
		} else if w.activeLimit > 0 && w.active(c.Group, "") >= w.activeLimit {
			return balance.PrepaymentReceipt{}, &balance.TaskLimitError{ActiveLimit: w.activeLimit}
		}
		if w.paused {
			return balance.PrepaymentReceipt{}, balance.ErrPrepaymentModelPaused
		}
		if w.balance(c.Group) < 200 {
			return balance.PrepaymentReceipt{}, balance.ErrPrepaymentInsufficientBalance
		}
		w.balances[c.Group] = w.balance(c.Group) - 200
		submission := "not_submitted"
		if c.Queue {
			submission = "waiting"
		}
		w.holds[c.BillingOperationID] = &queueHold{group: c.Group, status: "pending", submission: submission, createdAt: time.Now()}
		return w.receipt(c.BillingOperationID), nil
	}
	if hold == nil {
		return balance.PrepaymentReceipt{}, balance.ErrPrepaymentNotFound
	}
	final := hold.status != "pending"
	find := func(id string) *walletAttempt {
		for i := range hold.attempts {
			if hold.attempts[i].ID == id {
				return &hold.attempts[i]
			}
		}
		return nil
	}
	switch c.Action {
	case "begin_attempt":
		if final {
			return balance.PrepaymentReceipt{}, balance.ErrPrepaymentRejected
		}
		if find(c.AttemptID) != nil {
			return balance.PrepaymentReceipt{ID: c.BillingOperationID, Claimed: false}, nil
		}
		waiting := hold.submission == "waiting" && len(hold.attempts) == 0
		if hold.submission != "not_submitted" && !waiting {
			return balance.PrepaymentReceipt{}, balance.ErrPrepaymentRejected
		}
		if waiting {
			if w.balance(hold.group) < 0 {
				return balance.PrepaymentReceipt{}, balance.ErrPrepaymentInsufficientBalance
			}
			if w.paused {
				return balance.PrepaymentReceipt{}, balance.ErrPrepaymentModelPaused
			}
			if w.activeLimit > 0 && w.active(hold.group, c.BillingOperationID) >= w.activeLimit {
				return balance.PrepaymentReceipt{}, &balance.TaskLimitError{ActiveLimit: w.activeLimit}
			}
		}
		at := time.Now()
		hold.attempts = append(hold.attempts, walletAttempt{ID: c.AttemptID, State: "unknown"})
		hold.submission, hold.unknownSince = "unknown", &at
		return balance.PrepaymentReceipt{ID: c.BillingOperationID, Claimed: true}, nil
	case "reject_attempt", "accept_attempt":
		attempt := find(c.AttemptID)
		if attempt == nil {
			return balance.PrepaymentReceipt{}, balance.ErrPrepaymentRejected
		}
		state := map[string]string{"reject_attempt": "rejected", "accept_attempt": "accepted"}[c.Action]
		if attempt.State != "unknown" && attempt.State != state {
			return balance.PrepaymentReceipt{}, balance.ErrPrepaymentRejected
		}
		attempt.State = state
		if !final {
			hold.submission, hold.unknownSince = map[string]string{"rejected": "not_submitted", "accepted": "accepted"}[state], nil
		}
	case "settle":
		if !final {
			if c.Outcome.Reason == "not_accepted" && hold.submission == "accepted" {
				return balance.PrepaymentReceipt{}, balance.ErrPrepaymentRejected
			}
			if c.Outcome.Reason == "not_dispatched" && len(hold.attempts) > 0 {
				return balance.PrepaymentReceipt{}, balance.ErrPrepaymentRejected
			}
			hold.status = "refunded"
			w.balances[hold.group] = w.balance(hold.group) + 200
		}
	}
	return w.receipt(c.BillingOperationID), nil
}

func (w *queueWallet) receipt(id string) balance.PrepaymentReceipt {
	hold := w.holds[id]
	list := hold.attempts
	if list == nil {
		list = []walletAttempt{}
	}
	unknownSince := rawJSON(nil)
	if hold.unknownSince != nil {
		unknownSince = rawJSON(hold.unknownSince.UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	r := balance.PrepaymentReceipt{ID: id, Status: hold.status, PrepaidMicros: 200,
		SubmissionStatus: rawJSON(hold.submission), AttemptsJSON: rawJSON(string(rawJSON(list))),
		CreatedAt: rawJSON(hold.createdAt.UTC().Format("2006-01-02T15:04:05.000Z")), UnknownSince: unknownSince}
	if hold.status == "refunded" {
		zero, full := int64(0), int64(200)
		r.ChargedMicros, r.RefundMicros = &zero, &full
	}
	return r
}

// hold returns a copy of one hold's status and submission.
func (w *queueWallet) hold(id string) (status, submission string, attempts int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	hold := w.holds["native:"+id]
	if hold == nil {
		return "", "", 0
	}
	return hold.status, hold.submission, len(hold.attempts)
}

// actionsFor lists one task's wallet calls, settle outcomes spelled out.
func (w *queueWallet) actionsFor(id string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	actions := []string{}
	for _, c := range w.commands {
		if c.BillingOperationID != "native:"+id {
			continue
		}
		action := c.Action
		if c.Outcome != nil {
			action += ":" + c.Outcome.Kind + "/" + c.Outcome.Reason
		}
		if c.Action == "admit" && c.Queue {
			action += ":queue"
		}
		actions = append(actions, action)
	}
	return actions
}

func (w *queueWallet) count(action string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, c := range w.commands {
		if c.Action == action {
			n++
		}
	}
	return n
}

// queueProvider is a concurrency-safe provider that accepts every task.
type queueProvider struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (p *queueProvider) SubmitNative(_ context.Context, _ string, b []byte) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, string(b))
	if p.err != nil {
		return "", p.err
	}
	return "upstream-" + string(rune('a'+len(p.calls)-1)), nil
}

func (p *queueProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// queueEnv is an engine with the queue, a queue-aware wallet and provider.
type queueEnv struct {
	t        *testing.T
	e        *Engine
	w        *queueWallet
	p        *queueProvider
	plan     Plan
	enabled  bool
	limits   map[string]int
	timeout  time.Duration
	unusable bool
	mu       sync.Mutex
}

const falScope = "fal:default"

func newQueueEnv(t *testing.T) *queueEnv {
	t.Helper()
	queue = newQueueState()
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "queue.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}, &model.ImageTask{}))
	_, plan, _, _ := setup(t)
	env := &queueEnv{t: t, w: newQueueWallet(), p: &queueProvider{}, plan: plan, enabled: true,
		limits: map[string]int{falScope: 1}, timeout: 5 * time.Minute}
	env.e = &Engine{DB: db, Wallet: env.w, Provider: env.p, AbandonReservations: true,
		Queue: &QueueConfig{
			Enabled: func() bool { env.mu.Lock(); defer env.mu.Unlock(); return env.enabled },
			Limits:  func() map[string]int { env.mu.Lock(); defer env.mu.Unlock(); return env.limits },
			Timeout: func() time.Duration { env.mu.Lock(); defer env.mu.Unlock(); return env.timeout },
		},
		QueueProviders: map[string]QueueProvider{"fal": {
			Resolve: func(context.Context, *model.NativeTask) (Provider, error) {
				env.mu.Lock()
				defer env.mu.Unlock()
				if env.unusable {
					return nil, ErrChannelUnusable
				}
				return env.p, nil
			},
			OtherInFlight: model.ImageTasksInFlight(model.ChannelTypeFal),
		}}}
	return env
}

func (env *queueEnv) set(fn func()) {
	env.mu.Lock()
	defer env.mu.Unlock()
	fn()
}

// submit sends a task the way a customer request does.
func (env *queueEnv) submit(id, group string) (*model.NativeTask, error) {
	engine := *env.e
	engine.Provider = env.p
	return engine.Submit(context.Background(), id, group, 1, body, env.plan)
}

func (env *queueEnv) task(id, group string) *model.NativeTask {
	env.t.Helper()
	task, err := model.GetNativeTask(env.e.DB, id, group, 1)
	require.NoError(env.t, err)
	return task
}

// round runs one dispatcher round and waits for the tasks it claimed.
func (env *queueEnv) round() {
	var running sync.WaitGroup
	env.e.DispatchRound(context.Background(), &running)
	running.Wait()
}

// finish moves an accepted task out of provider capacity, as a delivered
// result would, and settles its hold.
func (env *queueEnv) finish(id string) {
	env.t.Helper()
	require.NoError(env.t, env.e.DB.Model(&model.NativeTask{}).Where("id = ?", id).
		Updates(map[string]any{"status": "completed", "billing_settled": true}).Error)
	env.w.mu.Lock()
	env.w.holds["native:"+id].status = "settled"
	env.w.mu.Unlock()
}

// http is a customer handler; the group is read from X-Group.
func (env *queueEnv) http() *HTTP {
	return &HTTP{Engine: env.e, limiter: newReadLimiter(time.Now),
		Identity: func(r *http.Request) (string, int, error) { return r.Header.Get("X-Group"), 1, nil },
		ResolvePlan: func(*http.Request, []byte) (Plan, Provider, error) {
			return env.plan, env.p, nil
		}}
}

func customerRequest(method, path, id, group string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(string(body)))
	r.Header.Set("X-Request-Id", id)
	r.Header.Set("X-Group", group)
	return r
}
