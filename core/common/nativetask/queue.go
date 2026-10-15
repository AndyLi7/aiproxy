package nativetask

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
)

// Own task queue (spec 2026-10-15): tasks over the provider's capacity are
// held in our database instead of refused or queued at the provider, and
// dispatched fairly between customers. Correctness lives in the database
// (compare-and-set transitions, leases) and the wallet; everything in this
// file's process state only saves wallet calls or reads.

// QueueConfig reads the queue settings; nil fields read the gateway options.
type QueueConfig struct {
	Enabled func() bool
	Limits  func() map[string]int
	Timeout func() time.Duration
}

// QueueProvider is what the queue needs from one provider (spec §11.4). The
// dispatcher never names a provider: it looks the task's route provider up.
type QueueProvider struct {
	// Resolve checks that the task's frozen channel is still usable and
	// returns the client to submit with. ErrChannelUnusable means it never
	// will be; any other error is transient.
	Resolve func(context.Context, *model.NativeTask) (Provider, error)
	// OtherInFlight counts the provider's work in a scope that is not a
	// native task (image tasks), per customer group.
	OtherInFlight model.OtherInFlight
}

// ErrChannelUnusable means a queued task's frozen channel was disabled,
// deleted, retargeted or rekeyed: it fails as model_unavailable_while_queued.
var ErrChannelUnusable = errors.New("frozen native channel unusable")

// queueScopePool is the pool of every scope in v1: one account per provider.
const queueScopePool = "default"

// QueueScope is the capacity pool of a provider route, "<provider>:<pool>".
func QueueScope(provider string) string {
	return provider + ":" + queueScopePool
}

// Backoffs after the wallet refused a dispatch (spec §6.3). They only save
// wallet calls; the wallet decides.
const (
	activeLimitBackoff  = 5 * time.Second
	pausedModelBackoff  = 30 * time.Second
	transientBackoff    = 5 * time.Second
	activeLimitCacheTTL = 10 * time.Minute
	positionCacheTTL    = 2 * time.Second
	waitingScopesTTL    = 2 * time.Second
	// queuedPollCeiling bounds recovery's poll delay in a scope that has
	// waiting tasks, so finished work frees its place quickly (spec §5.8).
	queuedPollCeiling = 5 * time.Second
	// reservedEnqueueGrace leaves a request that just admitted its queued
	// hold time to enqueue it itself before recovery does (spec §6.7).
	reservedEnqueueGrace = 10 * time.Second
)

func (e *Engine) queueEnabled() bool {
	if e.Queue != nil && e.Queue.Enabled != nil {
		return e.Queue.Enabled()
	}

	return config.GetNativeQueueEnabled()
}

// queueLimit is the scope's in-flight limit; 0 means unlimited.
func (e *Engine) queueLimit(scope string) int {
	var limits map[string]int
	if e.Queue != nil && e.Queue.Limits != nil {
		limits = e.Queue.Limits()
	} else {
		limits = config.GetNativeQueueLimits()
	}

	return limits[scope]
}

// queueTimeout is how long a task may wait; never above config.NativeQueueTimeout.
func (e *Engine) queueTimeout() time.Duration {
	timeout := config.GetNativeQueueTimeout()
	if e.Queue != nil && e.Queue.Timeout != nil {
		timeout = e.Queue.Timeout()
	}

	if timeout <= 0 || timeout > config.NativeQueueTimeout {
		return config.NativeQueueTimeout
	}

	return timeout
}

// scopeProvider is the provider part of a scope.
func scopeProvider(scope string) string {
	for i := 0; i < len(scope); i++ {
		if scope[i] == ':' {
			return scope[:i]
		}
	}

	return scope
}

func (e *Engine) otherInFlight(scope string) model.OtherInFlight {
	if provider, ok := e.QueueProviders[scopeProvider(scope)]; ok {
		return provider.OtherInFlight
	}

	return nil
}

// queueState is this process's view of the queue.
type queueState struct {
	mu  sync.Mutex
	now func() time.Time
	// backoff holds customers back after their own active limit refused a
	// dispatch; modelBackoff holds back one customer's tasks of one model
	// (paused model, unreadable channel) while their other tasks still go.
	backoff      map[string]time.Time
	modelBackoff map[string]time.Time
	limits       map[string]cachedLimit
	positions    map[string]cachedPositions
	scopes       cachedScopes
	expedited    map[string]time.Time
	wake         chan struct{}
}

type cachedLimit struct {
	limit int
	at    time.Time
}

type cachedPositions struct {
	at        time.Time
	positions map[string]int
}

type cachedScopes struct {
	at     time.Time
	scopes map[string]bool
}

func newQueueState() *queueState {
	return &queueState{now: time.Now, backoff: map[string]time.Time{}, modelBackoff: map[string]time.Time{}, limits: map[string]cachedLimit{},
		positions: map[string]cachedPositions{}, expedited: map[string]time.Time{}, wake: make(chan struct{}, 1)}
}

// queue is the process-wide state shared by requests, the dispatcher and recovery.
var queue = newQueueState()

// wakeDispatcher asks the dispatcher for a round now (a task entered the
// queue or left provider capacity).
func wakeDispatcher() {
	select {
	case queue.wake <- struct{}{}:
	default:
	}
}

func (q *queueState) backOff(group string, d time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()

	until := q.now().Add(d)
	if until.After(q.backoff[group]) {
		q.backoff[group] = until
	}
}

func (q *queueState) backedOff(group string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	until, ok := q.backoff[group]
	if ok && !q.now().Before(until) {
		delete(q.backoff, group)
		return false
	}

	return ok
}

func modelBackoffKey(group, model string) string { return group + "\x00" + model }

// backOffModel holds back a customer's tasks of one model.
func (q *queueState) backOffModel(group, model string, d time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()

	key := modelBackoffKey(group, model)
	until := q.now().Add(d)
	if until.After(q.modelBackoff[key]) {
		q.modelBackoff[key] = until
	}
}

// modelBackedOff reports a waiting task whose customer and model are held back.
func (q *queueState) modelBackedOff(entry model.NativeQueueEntry) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	key := modelBackoffKey(entry.GroupID, entry.Model)
	until, ok := q.modelBackoff[key]
	if ok && !q.now().Before(until) {
		delete(q.modelBackoff, key)
		return false
	}

	return ok
}

// rememberLimit keeps the active-task limit the wallet reported for a group.
func (q *queueState) rememberLimit(group string, limit int) {
	if limit <= 0 {
		return
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	q.limits[group] = cachedLimit{limit: limit, at: q.now()}
}

// activeLimit is the group's last reported active-task limit, 0 when unknown.
func (q *queueState) activeLimit(group string) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	cached, ok := q.limits[group]
	if !ok || q.now().Sub(cached.at) > activeLimitCacheTTL {
		delete(q.limits, group)
		return 0
	}

	return cached.limit
}

// fairOrder applies the fairness rules (spec §5.5) to one snapshot.
type fairOrder struct {
	scopeInFlight map[string]int64
	active        map[string]int64
	limit         func(string) int
	heads         map[string][]model.NativeQueueEntry
}

func newFairOrder(snapshot *model.NativeQueueSnapshot, limit func(string) int) *fairOrder {
	order := &fairOrder{scopeInFlight: map[string]int64{}, active: map[string]int64{}, limit: limit,
		heads: map[string][]model.NativeQueueEntry{}}
	for group, count := range snapshot.InFlightByGroup {
		order.scopeInFlight[group] = count
	}

	for group, count := range snapshot.ActiveByGroup {
		order.active[group] = count
	}

	// Waiting is oldest first, so each group's list is first in, first out.
	for _, entry := range snapshot.Waiting {
		order.heads[entry.GroupID] = append(order.heads[entry.GroupID], entry)
	}

	return order
}

// atLimit reports a group known to be at its own active-task limit.
func (o *fairOrder) atLimit(group string) bool {
	limit := o.limit(group)
	return limit > 0 && o.active[group] >= int64(limit)
}

// next pops the next task: rule 0 groups at their own active limit last (or,
// unless includeAtLimit, not at all), rule 1 fewest in flight in the scope,
// rule 2 earliest waiting task, rule 3 first in, first out within a group.
// skipGroup leaves customers out (their own active limit refused them);
// skipTask leaves single tasks out (their model paused or their channel
// unreadable), and the customer's next task takes their turn.
func (o *fairOrder) next(skipGroup func(string) bool, skipTask func(model.NativeQueueEntry) bool, includeAtLimit bool) (model.NativeQueueEntry, bool) {
	best, bestIndex, bestAtLimit := "", -1, false

	for group, queued := range o.heads {
		if len(queued) == 0 || (skipGroup != nil && skipGroup(group)) {
			continue
		}

		index := 0
		for skipTask != nil && index < len(queued) && skipTask(queued[index]) {
			index++
		}

		if index == len(queued) {
			continue
		}

		atLimit := o.atLimit(group)
		if atLimit && !includeAtLimit {
			continue
		}

		if best == "" || o.before(group, queued[index], atLimit, best, o.heads[best][bestIndex], bestAtLimit) {
			best, bestIndex, bestAtLimit = group, index, atLimit
		}
	}

	if best == "" {
		return model.NativeQueueEntry{}, false
	}

	queued := o.heads[best]
	entry := queued[bestIndex]
	rest := make([]model.NativeQueueEntry, 0, len(queued)-1)
	rest = append(rest, queued[:bestIndex]...)
	o.heads[best] = append(rest, queued[bestIndex+1:]...)
	o.scopeInFlight[best]++
	o.active[best]++

	return entry, true
}

func (o *fairOrder) before(group string, head model.NativeQueueEntry, atLimit bool, other string, otherHead model.NativeQueueEntry, otherAtLimit bool) bool {
	if atLimit != otherAtLimit {
		return !atLimit
	}

	if o.scopeInFlight[group] != o.scopeInFlight[other] {
		return o.scopeInFlight[group] < o.scopeInFlight[other]
	}

	if !head.EnqueuedAt.Equal(otherHead.EnqueuedAt) {
		return head.EnqueuedAt.Before(otherHead.EnqueuedAt)
	}

	return head.ID < otherHead.ID
}

// queuePositions simulates dispatch order on a snapshot (spec §5.6): the
// estimated place of every waiting task, 1 first.
func queuePositions(snapshot *model.NativeQueueSnapshot, limit func(string) int) map[string]int {
	order := newFairOrder(snapshot, limit)
	positions := make(map[string]int, len(snapshot.Waiting))

	for position := 1; ; position++ {
		entry, ok := order.next(nil, nil, true)
		if !ok {
			return positions
		}

		positions[entry.ID] = position
	}
}

// readQueueSnapshot reads a scope's queue; a seam for tests.
var readQueueSnapshot = model.ReadNativeQueueSnapshot

// QueuePosition is a waiting task's estimated place in its scope's queue, or
// 0 when it is not waiting or the estimate is unavailable. Estimates are
// cached per scope for two seconds; within them a task the estimate does not
// list (past the read limit) is not read again, unless it entered the queue
// after the estimate.
func (e *Engine) QueuePosition(task *model.NativeTask) int {
	if e == nil || e.DB == nil || task == nil || task.Status != model.NativeStatusWaiting || task.QueueScope == "" {
		return 0
	}

	queue.mu.Lock()
	cached, ok := queue.positions[task.QueueScope]
	fresh := ok && queue.now().Sub(cached.at) < positionCacheTTL
	queue.mu.Unlock()

	if _, listed := cached.positions[task.ID]; !fresh || (!listed && task.UpdatedAt.After(cached.at)) {
		snapshot, err := readQueueSnapshot(e.DB, task.QueueScope, queue.now(), e.otherInFlight(task.QueueScope))
		if err != nil {
			log.WithFields(log.Fields{"lane": "native", "scope": task.QueueScope}).WithError(err).Warn("native queue position unavailable")
			return 0
		}

		cached = cachedPositions{at: queue.now(), positions: queuePositions(snapshot, queue.activeLimit)}

		queue.mu.Lock()
		queue.positions[task.QueueScope] = cached
		queue.mu.Unlock()
	}

	return cached.positions[task.ID]
}

// scopeHasWaiting reports, from a set refreshed every two seconds, whether a
// scope has waiting tasks.
func (e *Engine) scopeHasWaiting(scope string) bool {
	if scope == "" {
		return false
	}

	queue.mu.Lock()
	cached := queue.scopes
	stale := cached.scopes == nil || queue.now().Sub(cached.at) >= waitingScopesTTL
	queue.mu.Unlock()

	if stale {
		scopes, err := model.NativeQueueScopesWaiting(e.DB)
		if err != nil {
			return false
		}

		cached = cachedScopes{at: queue.now(), scopes: map[string]bool{}}
		for _, waiting := range scopes {
			cached.scopes[waiting] = true
		}

		queue.mu.Lock()
		queue.scopes = cached
		queue.mu.Unlock()
	}

	return cached.scopes[scope]
}

// claimToken is a random dispatch claim token.
func claimToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}

	return "dispatch-" + hex.EncodeToString(raw[:]), nil
}

// claim takes one waiting task of a scope by the fairness rules, or only
// onlyID while it is the scope's single waiting task (the fast path). noRoom
// reports that the scope's in-flight limit is reached.
func (e *Engine) claim(scope, onlyID string) (*model.NativeTask, bool, error) {
	owner, err := claimToken()
	if err != nil {
		return nil, false, err
	}

	return model.ClaimNativeDispatch(e.DB, model.NativeDispatchClaim{
		Scope: scope, Limit: e.queueLimit(scope), Owner: owner, Now: queue.now(), OnlyID: onlyID,
		Other: e.otherInFlight(scope),
		Pick: func(snapshot *model.NativeQueueSnapshot) string {
			entry, ok := newFairOrder(snapshot, queue.activeLimit).next(queue.backedOff, queue.modelBackedOff, false)
			if !ok {
				return ""
			}

			return entry.ID
		},
	})
}

// expedite brings the scope's accepted tasks' next poll within
// queuedPollCeiling, at most once per ceiling (spec §5.8).
func (e *Engine) expedite(scope string) {
	queue.mu.Lock()
	last := queue.expedited[scope]
	due := queue.now().Sub(last) >= queuedPollCeiling
	if due {
		queue.expedited[scope] = queue.now()
	}
	queue.mu.Unlock()

	if !due {
		return
	}

	if err := model.ExpediteNativeRecovery(e.DB, scope, queue.now().Add(queuedPollCeiling)); err != nil {
		log.WithFields(log.Fields{"lane": "native", "scope": scope}).WithError(err).Warn("native queue: polls not expedited")
	}
}

// QueueStatusScope is one scope's line of the admin queue status.
type QueueStatusScope struct {
	Scope string `json:"scope"`
	// Limit is the in-flight limit; null means unlimited.
	Limit            *int   `json:"limit"`
	InFlight         int64  `json:"inFlight"`
	Waiting          int64  `json:"waiting"`
	OldestEnqueuedAt *int64 `json:"oldestEnqueuedAt"`
}

// QueueStatus reports the queue settings and every scope's line.
type QueueStatus struct {
	Enabled        bool               `json:"enabled"`
	TimeoutSeconds int64              `json:"timeoutSeconds"`
	Scopes         []QueueStatusScope `json:"scopes"`
}

// ReadQueueStatus is the admin view of the queue (spec §7.3).
func (e *Engine) ReadQueueStatus() (QueueStatus, error) {
	status := QueueStatus{Enabled: e.queueEnabled(), TimeoutSeconds: int64(e.queueTimeout() / time.Second), Scopes: []QueueStatusScope{}}

	var limits map[string]int
	if e.Queue != nil && e.Queue.Limits != nil {
		limits = e.Queue.Limits()
	} else {
		limits = config.GetNativeQueueLimits()
	}

	configured := make([]string, 0, len(limits)+len(e.QueueProviders))
	for scope := range limits {
		configured = append(configured, scope)
	}

	for provider := range e.QueueProviders {
		configured = append(configured, QueueScope(provider))
	}

	sort.Strings(configured)

	lines, err := model.ReadNativeQueueStatus(e.DB, configured, e.otherInFlight)
	if err != nil {
		return status, err
	}

	for _, line := range lines {
		entry := QueueStatusScope{Scope: line.Scope, InFlight: line.InFlight, Waiting: line.Waiting}
		if limit, ok := limits[line.Scope]; ok {
			entry.Limit = &limit
		}

		if line.OldestEnqueuedAt != nil {
			at := line.OldestEnqueuedAt.UnixMilli()
			entry.OldestEnqueuedAt = &at
		}

		status.Scopes = append(status.Scopes, entry)
	}

	return status, nil
}
