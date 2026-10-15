package model

import (
	"sort"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// Own task queue (spec 2026-10-15). A queued task holds money in the wallet
// but was never sent: the only provider call still follows a
// compare-and-set into submitting, now from reserved or from dispatching, so
// reserved, waiting and dispatching all prove nothing was sent.
const (
	// NativeStatusWaiting is a task held in our queue, not yet sent.
	NativeStatusWaiting = "waiting"
	// NativeStatusDispatching is a task a dispatcher claimed from the queue
	// and has not sent yet; its claim is leased (DispatchOwner, DispatchUntil).
	NativeStatusDispatching = "dispatching"
)

// Failure codes of a queued task that was never sent. Each is refunded in
// full: settle failed/not_dispatched while the wallet holds no attempt. Every
// move to failed below makes the task due for recovery at once, which retries
// a refund that failed right after it.
const (
	CancelledCode                   = "cancelled"
	QueueTimeoutCode                = "queue_timeout"
	ModelUnavailableWhileQueuedCode = "model_unavailable_while_queued"
	InsufficientBalanceCode         = "insufficient_balance"
)

// NativeDispatchLease bounds one claim from the queue: the wallet claim plus
// the provider submission. Recovery fails a claim still held after it.
const NativeDispatchLease = 2 * time.Minute

// NativeQueueReadLimit bounds the waiting rows one queue decision reads.
const NativeQueueReadLimit = 1000

// NativeInFlightStatuses occupy provider capacity (spec §5.3). The provider's
// own queue (queued) counts on purpose: within our limit the provider never
// queues for us, so its generation deadline only counts generation.
var NativeInFlightStatuses = []string{NativeStatusDispatching, "submitting", "submission_unknown", "queued", "running"}

// nativeActiveStatuses approximate the wallet's active-task count of a
// customer: work sent and not finished, plus results not yet delivered.
// Completed tasks still waiting for their bill are added by billing_settled.
var nativeActiveStatuses = []string{NativeStatusDispatching, "submitting", "submission_unknown", "queued", "running", "result_received", "delivery_ready"}

// ImageTaskInFlightStatuses are image tasks sent and not finished.
var ImageTaskInFlightStatuses = []string{"submitting", "submission_unknown", "queued", "in_progress"}

// NativeQueueEntry is one waiting task.
type NativeQueueEntry struct {
	ID         string
	GroupID    string
	TokenID    int
	EnqueuedAt time.Time
}

// NativeQueueSnapshot is what a queue decision reads for one scope.
type NativeQueueSnapshot struct {
	Scope string
	// Waiting is ordered by enqueue time, oldest first (at most
	// NativeQueueReadLimit rows; Truncated reports more).
	Waiting   []NativeQueueEntry
	Truncated bool
	// InFlight is the scope's work occupying provider capacity, native and
	// other (image tasks), and InFlightByGroup the same per customer.
	InFlight        int64
	InFlightByGroup map[string]int64
	// ActiveByGroup approximates, for each customer with waiting tasks, the
	// wallet's count of their active tasks in every scope.
	ActiveByGroup map[string]int64
}

// OtherInFlight counts, per customer group, work in a scope that is not a
// native task (image tasks). It is registered per provider.
type OtherInFlight func(tx *gorm.DB, scope string) (map[string]int64, error)

// ReadNativeQueueSnapshot reads the scope's queue and in-flight work.
func ReadNativeQueueSnapshot(db *gorm.DB, scope string, now time.Time, other OtherInFlight) (*NativeQueueSnapshot, error) {
	snapshot := &NativeQueueSnapshot{Scope: scope, InFlightByGroup: map[string]int64{}, ActiveByGroup: map[string]int64{}}

	var waiting []NativeTask
	if err := db.Select("id", "group_id", "token_id", "enqueued_at").
		Where("status = ? AND queue_scope = ? AND (dispatch_after IS NULL OR dispatch_after <= ?)", NativeStatusWaiting, scope, now.UTC()).
		Order("enqueued_at ASC, id ASC").Limit(NativeQueueReadLimit + 1).Find(&waiting).Error; err != nil {
		return nil, err
	}

	if len(waiting) > NativeQueueReadLimit {
		waiting = waiting[:NativeQueueReadLimit]
		snapshot.Truncated = true
	}

	groups := map[string]struct{}{}

	for _, task := range waiting {
		entry := NativeQueueEntry{ID: task.ID, GroupID: task.GroupID, TokenID: task.TokenID}
		if task.EnqueuedAt != nil {
			entry.EnqueuedAt = task.EnqueuedAt.UTC()
		}

		snapshot.Waiting = append(snapshot.Waiting, entry)
		groups[task.GroupID] = struct{}{}
	}

	type groupCount struct {
		GroupID string
		Count   int64
	}

	var native []groupCount
	if err := db.Model(&NativeTask{}).Select("group_id, count(*) AS count").
		Where("queue_scope = ? AND status IN ?", scope, NativeInFlightStatuses).
		Group("group_id").Scan(&native).Error; err != nil {
		return nil, err
	}

	for _, row := range native {
		snapshot.InFlightByGroup[row.GroupID] += row.Count
		snapshot.InFlight += row.Count
	}

	if other != nil {
		counts, err := other(db, scope)
		if err != nil {
			return nil, err
		}

		for group, count := range counts {
			snapshot.InFlightByGroup[group] += count
			snapshot.InFlight += count
		}
	}

	if len(groups) == 0 {
		return snapshot, nil
	}

	names := make([]string, 0, len(groups))
	for group := range groups {
		names = append(names, group)
	}

	sort.Strings(names)

	var active []groupCount
	if err := db.Model(&NativeTask{}).Select("group_id, count(*) AS count").
		Where("group_id IN ? AND (status IN ? OR (status = ? AND billing_settled = ?))", names, nativeActiveStatuses, "completed", false).
		Group("group_id").Scan(&active).Error; err != nil {
		return nil, err
	}

	for _, row := range active {
		snapshot.ActiveByGroup[row.GroupID] += row.Count
	}

	var images []groupCount
	if err := db.Model(&ImageTask{}).Select("group_id, count(*) AS count").
		Where("group_id IN ? AND status IN ?", names, ImageTaskInFlightStatuses).
		Group("group_id").Scan(&images).Error; err != nil {
		return nil, err
	}

	for _, row := range images {
		snapshot.ActiveByGroup[row.GroupID] += row.Count
	}

	return snapshot, nil
}

// ImageTasksInFlight counts a channel type's image tasks in flight per group:
// the OtherInFlight of a provider whose image tasks share its capacity.
func ImageTasksInFlight(channelType ChannelType) OtherInFlight {
	return func(tx *gorm.DB, _ string) (map[string]int64, error) {
		var rows []struct {
			GroupID string
			Count   int64
		}
		if err := tx.Model(&ImageTask{}).Select("group_id, count(*) AS count").
			Where("channel_type = ? AND status IN ?", channelType, ImageTaskInFlightStatuses).
			Group("group_id").Scan(&rows).Error; err != nil {
			return nil, err
		}

		counts := make(map[string]int64, len(rows))
		for _, row := range rows {
			counts[row.GroupID] = row.Count
		}

		return counts, nil
	}
}

// EnqueueNativeTask moves a reserved task whose wallet hold is waiting into
// the queue of scope: reserved -> waiting, the same guard as the submit
// transition. Recovery next looks at it when its queue timeout is due (wakeAt).
func EnqueueNativeTask(db *gorm.DB, id, group string, token int, scope string, enqueuedAt, wakeAt time.Time) error {
	if scope == "" {
		return ErrNativeTaskConflict
	}

	at := enqueuedAt.UTC()
	result := db.Model(&NativeTask{}).
		Where("id = ? AND group_id = ? AND token_id = ? AND status = ? AND upstream_id = ?", id, group, token, "reserved", "").
		Updates(map[string]any{
			"status": NativeStatusWaiting, "queue_scope": scope, "enqueued_at": at, "dispatch_after": at,
			"next_recovery_at": wakeAt.Unix(), "updated_at": time.Now(),
		})
	if result.Error != nil {
		return result.Error
	}

	if result.RowsAffected != 1 {
		return ErrNativeTaskConflict
	}

	return nil
}

// NativeDispatchClaim asks for one task of a scope.
type NativeDispatchClaim struct {
	Scope string
	// Limit is the scope's in-flight limit; 0 means unlimited.
	Limit int
	// Owner is the claim's random token.
	Owner string
	Now   time.Time
	// OnlyID restricts the claim to this task, and only while no other task
	// of the scope waits (the submitting request's fast path, spec §2 step 5).
	OnlyID string
	Other  OtherInFlight
	// Pick chooses the next task by the fairness rules; "" claims none.
	Pick func(*NativeQueueSnapshot) string
}

// claimLocks serialise claims of one scope inside this process. PostgreSQL
// also takes a transaction advisory lock, so claims across instances and
// overlapping deployments never exceed the limit; SQLite serialises writers.
var claimLocks sync.Map

// claimLocally takes claimLocks; tests turn it off to exercise the database
// lock alone, as separate instances would.
var claimLocally = true

// ClaimNativeDispatch atomically claims one waiting task of a scope when it
// has room (spec §5.4): lock the scope, count its in-flight work, pick a task,
// then waiting -> dispatching with the lease. A nil task means no room
// (noRoom) or no task to claim.
func ClaimNativeDispatch(db *gorm.DB, claim NativeDispatchClaim) (task *NativeTask, noRoom bool, err error) {
	if claim.Scope == "" || claim.Owner == "" || len(claim.Owner) > 64 || claim.Pick == nil {
		return nil, false, ErrNativeTaskConflict
	}

	if claimLocally {
		lock, _ := claimLocks.LoadOrStore(claim.Scope, &sync.Mutex{})
		lock.(*sync.Mutex).Lock()
		defer lock.(*sync.Mutex).Unlock()
	}

	var claimed *NativeTask

	err = db.Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", "native_queue:"+claim.Scope).Error; err != nil {
				return err
			}
		}

		snapshot, err := ReadNativeQueueSnapshot(tx, claim.Scope, claim.Now, claim.Other)
		if err != nil {
			return err
		}

		if snapshot.Truncated {
			log.WithFields(log.Fields{"lane": "native", "scope": claim.Scope}).
				Warn("native queue holds more waiting tasks than one decision reads")
		}

		if claim.Limit > 0 && snapshot.InFlight >= int64(claim.Limit) {
			noRoom = true
			return nil
		}

		if claim.OnlyID != "" && (len(snapshot.Waiting) != 1 || snapshot.Waiting[0].ID != claim.OnlyID) {
			return nil
		}

		id := claim.Pick(snapshot)
		if id == "" {
			return nil
		}

		var picked *NativeQueueEntry

		for i := range snapshot.Waiting {
			if snapshot.Waiting[i].ID == id {
				picked = &snapshot.Waiting[i]
				break
			}
		}

		if picked == nil {
			return nil
		}

		now := claim.Now.UTC()
		until := now.Add(NativeDispatchLease)

		result := tx.Model(&NativeTask{}).
			Where("id = ? AND group_id = ? AND token_id = ? AND status = ? AND upstream_id = ?", picked.ID, picked.GroupID, picked.TokenID, NativeStatusWaiting, "").
			Updates(map[string]any{
				"status": NativeStatusDispatching, "dispatch_owner": claim.Owner, "dispatch_until": until,
				"dispatched_at": now, "next_recovery_at": until.Unix(), "updated_at": time.Now(),
			})
		if result.Error != nil {
			return result.Error
		}

		if result.RowsAffected != 1 {
			return nil
		}

		var task NativeTask
		if err := tx.Where("id = ? AND group_id = ? AND token_id = ?", picked.ID, picked.GroupID, picked.TokenID).First(&task).Error; err != nil {
			return err
		}

		claimed = &task

		return nil
	})

	return claimed, noRoom, err
}

// dispatchHeld is the guard of every move out of dispatching by its holder.
func dispatchHeld(db *gorm.DB, id, group string, token int, owner string) *gorm.DB {
	return db.Model(&NativeTask{}).Where("id = ? AND group_id = ? AND token_id = ? AND status = ? AND upstream_id = ? AND dispatch_owner = ?",
		id, group, token, NativeStatusDispatching, "", owner)
}

// ReturnNativeDispatch puts a claimed task back in the queue: dispatching ->
// waiting, only when the wallet refused its claim (active limit, paused
// model), which leaves the hold unchanged. Its place (enqueued_at) is kept.
func ReturnNativeDispatch(db *gorm.DB, id, group string, token int, owner string, wakeAt time.Time) error {
	result := dispatchHeld(db, id, group, token, owner).Updates(map[string]any{
		"status": NativeStatusWaiting, "dispatch_owner": "", "dispatch_until": nil, "dispatched_at": nil,
		"next_recovery_at": wakeAt.Unix(), "updated_at": time.Now(),
	})
	if result.Error != nil {
		return result.Error
	}

	if result.RowsAffected != 1 {
		return ErrNativeTaskConflict
	}

	return nil
}

// StartNativeDispatchSubmission lets the holder of a live claim send the task:
// dispatching -> submitting while the lease holds. created_at becomes the
// submission time, as for reserved -> submitting. A claim whose lease ran out
// matches no row and must not call the provider.
func StartNativeDispatchSubmission(db *gorm.DB, id, group string, token int, owner string, now time.Time) error {
	result := dispatchHeld(db, id, group, token, owner).Where("dispatch_until > ?", now.UTC()).
		Updates(map[string]any{"status": "submitting", "error_code": "", "created_at": time.Now(), "updated_at": time.Now()})
	if result.Error != nil {
		return result.Error
	}

	if result.RowsAffected != 1 {
		return ErrNativeTaskConflict
	}

	return nil
}

// FailNativeDispatch ends a claimed task that will not be sent, by its holder.
func FailNativeDispatch(db *gorm.DB, id, group string, token int, owner, code string) error {
	result := dispatchHeld(db, id, group, token, owner).
		Updates(map[string]any{"status": "failed", "error_code": code, "public_error": "", "next_recovery_at": time.Now().Unix(), "updated_at": time.Now()})
	if result.Error != nil {
		return result.Error
	}

	if result.RowsAffected != 1 {
		return ErrNativeTaskConflict
	}

	return nil
}

// ExpireNativeDispatch fails a claim whose lease ran out (recovery, spec §6.6):
// the holder died or the wallet stalled. It is never put back in the queue.
// owner is the claim recovery read, so a newer claim is never touched.
func ExpireNativeDispatch(db *gorm.DB, id, group string, token int, owner string, now time.Time) (bool, error) {
	result := dispatchHeld(db, id, group, token, owner).Where("dispatch_until <= ?", now.UTC()).
		Updates(map[string]any{"status": "failed", "error_code": NotStartedCode, "public_error": "", "next_recovery_at": time.Now().Unix(), "updated_at": time.Now()})

	return result.RowsAffected == 1, result.Error
}

// CancelNativeTask fails a waiting task at its customer's request. It and a
// claim exclude each other: only one moves the row out of waiting.
func CancelNativeTask(db *gorm.DB, id, group string, token int) (bool, error) {
	result := db.Model(&NativeTask{}).
		Where("id = ? AND group_id = ? AND token_id = ? AND status = ? AND upstream_id = ?", id, group, token, NativeStatusWaiting, "").
		Updates(map[string]any{"status": "failed", "error_code": CancelledCode, "public_error": "", "next_recovery_at": time.Now().Unix(), "updated_at": time.Now()})

	return result.RowsAffected == 1, result.Error
}

// ExpireNativeQueueWait fails a task still waiting since enqueuedBefore.
func ExpireNativeQueueWait(db *gorm.DB, id, group string, token int, enqueuedBefore time.Time) (bool, error) {
	result := db.Model(&NativeTask{}).
		Where("id = ? AND group_id = ? AND token_id = ? AND status = ? AND upstream_id = ? AND enqueued_at <= ?",
			id, group, token, NativeStatusWaiting, "", enqueuedBefore.UTC()).
		Updates(map[string]any{"status": "failed", "error_code": QueueTimeoutCode, "public_error": "", "next_recovery_at": time.Now().Unix(), "updated_at": time.Now()})

	return result.RowsAffected == 1, result.Error
}

// NativeQueueScopesWaiting lists the scopes that have waiting tasks.
func NativeQueueScopesWaiting(db *gorm.DB) ([]string, error) {
	var scopes []string
	err := db.Model(&NativeTask{}).Distinct("queue_scope").
		Where("status = ? AND queue_scope <> ?", NativeStatusWaiting, "").Order("queue_scope").Pluck("queue_scope", &scopes).Error

	return scopes, err
}

// ExpediteNativeRecovery brings the next poll of a scope's accepted tasks to
// at the latest, so a task the provider finished frees its place sooner while
// others wait (spec §5.8).
func ExpediteNativeRecovery(db *gorm.DB, scope string, at time.Time) error {
	return db.Model(&NativeTask{}).
		Where("queue_scope = ? AND status IN ? AND next_recovery_at > ?", scope, []string{"queued", "running"}, at.Unix()).
		Update("next_recovery_at", at.Unix()).Error
}

// NativeQueueScopeStatus is one scope's line of the admin queue status.
type NativeQueueScopeStatus struct {
	Scope            string
	InFlight         int64
	Waiting          int64
	OldestEnqueuedAt *time.Time
}

// ReadNativeQueueStatus reports in-flight and waiting work of each scope in
// scopes and of every scope with waiting or in-flight native tasks.
func ReadNativeQueueStatus(db *gorm.DB, scopes []string, other func(scope string) OtherInFlight) ([]NativeQueueScopeStatus, error) {
	var present []string
	if err := db.Model(&NativeTask{}).Distinct("queue_scope").
		Where("queue_scope <> ? AND status IN ?", "", append([]string{NativeStatusWaiting}, NativeInFlightStatuses...)).
		Pluck("queue_scope", &present).Error; err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	all := []string{}

	for _, scope := range append(append([]string{}, scopes...), present...) {
		if _, ok := seen[scope]; !ok && scope != "" {
			seen[scope] = struct{}{}
			all = append(all, scope)
		}
	}

	sort.Strings(all)

	report := make([]NativeQueueScopeStatus, 0, len(all))

	for _, scope := range all {
		var counter OtherInFlight
		if other != nil {
			counter = other(scope)
		}

		snapshot, err := ReadNativeQueueSnapshot(db, scope, time.Now().Add(24*time.Hour), counter)
		if err != nil {
			return nil, err
		}

		line := NativeQueueScopeStatus{Scope: scope, InFlight: snapshot.InFlight}

		if err := db.Model(&NativeTask{}).Where("status = ? AND queue_scope = ?", NativeStatusWaiting, scope).
			Count(&line.Waiting).Error; err != nil {
			return nil, err
		}

		if len(snapshot.Waiting) > 0 && !snapshot.Waiting[0].EnqueuedAt.IsZero() {
			oldest := snapshot.Waiting[0].EnqueuedAt
			line.OldestEnqueuedAt = &oldest
		}

		report = append(report, line)
	}

	return report, nil
}
