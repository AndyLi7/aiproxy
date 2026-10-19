package model

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/labring/aiproxy/core/relay/mode"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Task view (C step 2, batch 1): one row per generation task, with no
// migration and no new index.
//
//   - SUPERSEDED: a failed native submit whose billing operation also appears
//     on the task's own row or on an earlier failed submit. The task's money
//     is on that other row, so this row is not a task.
//   - TASK = BASE_CALL AND status <> rejected AND NOT SUPERSEDED. BASE_CALL is
//     isCallExpr for customers and adminLogCallExpr for the admin log.
//   - kind=tasks lists TASK; kind=non_task lists VISIBLE AND NOT TASK, where
//     VISIBLE is request_source <> admin_demo for customers and every row for
//     the admin log. The two kinds partition the visible rows.
//
// Every fragment is written once in SQL and once in Go, never evaluates to
// NULL (COALESCE wraps the outer row's columns only), and runs unchanged on
// Postgres, SQLite and MySQL: no string functions, no intervals, no LIKE on
// IDs. Inner join keys use plain = so the partial index
// idx_logs_group_billing_op and the task tables' primary keys stay usable.
// Outer columns inside a fragment with a subquery are written logs.<column>;
// the unqualified call and status expressions never go inside a subquery.

// LogTaskViewFeature tells the App that /api/log/:group, /api/logs/ and
// /api/logs/search accept kind=tasks and kind=non_task, and that
// /api/usage/:group/{calls,keys} accept unit=tasks. It is announced only when
// the task view's storage and indexes are ready (TaskViewStorageReady).
const LogTaskViewFeature = "log_task_view_v1"

const (
	// nativeSubmitEndpoint is the endpoint of a middleware row for a native
	// submission (failed or rejected) and of a native task's own row.
	nativeSubmitEndpoint = "POST /v1/model-tasks"
	// imageSubmitEndpoint is the endpoint of a middleware row for an image
	// task submission; imageTaskRowEndpoint is the image task's own row.
	imageSubmitEndpoint  = "POST /v1/images/tasks"
	imageTaskRowEndpoint = "/v1/images/tasks"

	// errorCodeRequestIDConflict marks a submit whose body differs from the
	// task's: a different generation, never folded under the task.
	errorCodeRequestIDConflict = "request_id_conflict"

	nativeBillingOperationPrefix = "native:"
)

// supersededExpr is the SQL form of Log.supersededAmong.
func supersededExpr() (string, []any) {
	return "(COALESCE(logs.endpoint, '') = ?" +
			" AND COALESCE(logs.async_usage_status, 0) = ?" +
			" AND COALESCE(logs.billing_operation_id, '') <> ''" +
			" AND EXISTS (SELECT 1 FROM logs x" +
			" WHERE x.group_id = logs.group_id" +
			" AND x.billing_operation_id = logs.billing_operation_id" +
			" AND (COALESCE(x.async_usage_status, 0) <> ? OR x.id < logs.id)))",
		[]any{nativeSubmitEndpoint, AsyncUsageStatusNone, AsyncUsageStatusNone}
}

// supersededAmong reports whether the row is a failed native submit whose op
// also appears on the task's own row (async status set) or on an earlier row.
// peers are rows of any group; the earliest failed submit stays the task when
// no own row exists, so N rows sharing one op leave exactly one task.
func (l *Log) supersededAmong(peers []*Log) bool {
	if string(l.Endpoint) != nativeSubmitEndpoint ||
		l.AsyncUsageStatus != AsyncUsageStatusNone ||
		l.BillingOperationID == "" {
		return false
	}

	for _, peer := range peers {
		if peer == nil || peer.ID == l.ID {
			continue
		}

		if peer.GroupID == l.GroupID && peer.BillingOperationID == l.BillingOperationID &&
			(peer.AsyncUsageStatus != AsyncUsageStatusNone || peer.ID < l.ID) {
			return true
		}
	}

	return false
}

// taskViewTaskExpr is the SQL form of Log.isTaskViewTask. now only feeds the
// status expression; whether a row is rejected does not depend on it.
func taskViewTaskExpr(admin bool, now time.Time) (string, []any) {
	baseSQL, baseArgs := isCallExpr()
	if admin {
		baseSQL, baseArgs = adminLogCallExpr()
	}

	statusSQL, statusArgs := callStatusExpr(now)
	supersededSQL, supersededArgs := supersededExpr()

	args := make([]any, 0, len(baseArgs)+len(statusArgs)+1+len(supersededArgs))
	args = append(args, baseArgs...)
	args = append(args, statusArgs...)
	args = append(args, string(CallStatusRejected))
	args = append(args, supersededArgs...)

	return "(" + baseSQL + ") AND (" + statusSQL + ") <> ? AND NOT " + supersededSQL, args
}

// isTaskViewTask reports whether the row is a task row (kind=tasks). peers
// are the rows supersededAmong compares against.
func (l *Log) isTaskViewTask(admin bool, peers []*Log) bool {
	base := l.IsCall()
	if admin {
		base = l.isAdminLogCall()
	}

	return base && l.CallStatus(time.Now()) != CallStatusRejected && !l.supersededAmong(peers)
}

// customerVisibleExpr is the SQL form of the customer list's VISIBLE: every
// row but internal demos.
func customerVisibleExpr() (string, []any) {
	return "COALESCE(request_source, '') <> ?", []any{RequestSourceAdminDemo}
}

// taskViewNonTaskExpr is VISIBLE AND NOT TASK: the exact complement of
// kind=tasks among the rows the list may show.
func taskViewNonTaskExpr(admin bool, now time.Time) (string, []any) {
	taskSQL, taskArgs := taskViewTaskExpr(admin, now)
	if admin {
		return "NOT (" + taskSQL + ")", taskArgs
	}

	visibleSQL, visibleArgs := customerVisibleExpr()

	return visibleSQL + " AND NOT (" + taskSQL + ")", append(visibleArgs, taskArgs...)
}

// taskViewKindAllowsStatus: a task row is never rejected, and the non-task
// list takes no status filter.
func taskViewKindAllowsStatus(tasks, nonTask bool, status CallStatus) bool {
	switch {
	case nonTask:
		return status == ""
	case tasks:
		return status != CallStatusRejected
	default:
		return true
	}
}

// TaskView reports kind=tasks and kind=non_task.
func (k GroupLogKind) TaskView() bool {
	return k == GroupLogKindTasks || k == GroupLogKindNonTask
}

// AllowsStatus reports whether the status filter applies to this kind.
func (k GroupLogKind) AllowsStatus(status CallStatus) bool {
	return taskViewKindAllowsStatus(k == GroupLogKindTasks, k == GroupLogKindNonTask, status)
}

// TaskView reports kind=tasks and kind=non_task.
func (k AdminLogKind) TaskView() bool {
	return k == AdminLogKindTasks || k == AdminLogKindNonTask
}

// AllowsStatus reports whether the status filter applies to this kind.
func (k AdminLogKind) AllowsStatus(status CallStatus) bool {
	return taskViewKindAllowsStatus(k == AdminLogKindTasks, k == AdminLogKindNonTask, status)
}

// CallUnit selects what the usage counts count.
//
//   - requests (default): every call row.
//   - tasks: SUPERSEDED rows are left out, so a submit that failed after the
//     hold and was resent with the same ID counts once. Rejected calls are
//     never folded: each counts in its own window, so a past window's count
//     never changes later.
type CallUnit string

const (
	CallUnitRequests CallUnit = "requests"
	CallUnitTasks    CallUnit = "tasks"
)

// ParseCallUnit accepts "", requests or tasks; "" means requests.
func ParseCallUnit(raw string) (CallUnit, error) {
	switch CallUnit(raw) {
	case "", CallUnitRequests:
		return CallUnitRequests, nil
	case CallUnitTasks:
		return CallUnitTasks, nil
	default:
		return "", errors.New("invalid unit parameter")
	}
}

// taskLane is the task table a native or image task row belongs to.
type taskLane int

const (
	taskLaneNone taskLane = iota
	taskLaneNative
	taskLaneImage
)

// taskLaneOf returns the lane and task ID of a native or image task row. A
// native row's ID comes from its op when it has one: the failed submit of a
// 1-7 character ID carries a timestamp request ID (middleware/reqid.go).
func (l *Log) taskLaneOf() (taskLane, string) {
	endpoint := string(l.Endpoint)

	switch {
	case l.Mode == int(mode.NativeTasks) || endpoint == nativeSubmitEndpoint:
		if id, ok := strings.CutPrefix(
			string(l.BillingOperationID),
			nativeBillingOperationPrefix,
		); ok &&
			id != "" {
			return taskLaneNative, id
		}

		return taskLaneNative, string(l.RequestID)
	case endpoint == imageTaskRowEndpoint:
		return taskLaneImage, string(l.RequestID)
	default:
		return taskLaneNone, ""
	}
}

// foldableExpr selects the rejected submits of one lane that belong to a task
// of the same group and key with a visible row (batch 2 FOLDABLE, used by
// GetTaskRequests). A 409 request_id_conflict is a different generation and
// never folds. The row's own request_id must equal the task ID: image
// handler rejections carry image-trace IDs and never match.
func foldableExpr(lane taskLane, now time.Time) (string, []any) {
	table, endpoint := "native_tasks", nativeSubmitEndpoint
	if lane == taskLaneImage {
		table, endpoint = "image_tasks", imageSubmitEndpoint
	}

	statusSQL, statusArgs := callStatusExpr(now)

	args := make([]any, 0, len(statusArgs)+4)
	args = append(args, endpoint, AsyncUsageStatusNone)
	args = append(args, statusArgs...)
	args = append(args, string(CallStatusRejected), errorCodeRequestIDConflict)

	return "(COALESCE(logs.endpoint, '') = ?" +
		" AND COALESCE(logs.async_usage_status, 0) = ?" +
		" AND (" + statusSQL + ") = ?" +
		" AND COALESCE(logs.error_code, '') <> ?" +
		" AND EXISTS (SELECT 1 FROM " + table + " t" +
		" WHERE t.id = logs.request_id AND t.group_id = logs.group_id AND t.token_id = logs.token_id" +
		" AND EXISTS (SELECT 1 FROM logs y" +
		" WHERE y.group_id = t.group_id AND y.billing_operation_id = t.billing_operation_id)))", args
}

// TaskRequestRelation says how a related request belongs to a task.
type TaskRequestRelation string

const (
	// TaskRequestRejected: a submit with the task's ID, group and key that a
	// gate refused (nothing held).
	TaskRequestRejected TaskRequestRelation = "rejected"
	// TaskRequestFailedSubmit: a submit carrying the task's billing operation
	// that failed after the hold; any charge is on the task.
	TaskRequestFailedSubmit TaskRequestRelation = "failed_submit"

	// TaskRequestHintBeforeSubmit marks a rejected attempt sent before the
	// task was created.
	TaskRequestHintBeforeSubmit = "before_submit"
)

// TaskRequest is one request related to a task.
type TaskRequest struct {
	Log      *Log                `json:"log"`
	Relation TaskRequestRelation `json:"relation"`
	Hint     string              `json:"hint,omitempty"`
}

// TaskRequests are the requests related to one task row. Truncated: a lookup
// hit its row limit or its time limit, so the list may be incomplete.
type TaskRequests struct {
	Task      *Log          `json:"task"`
	Attempts  []TaskRequest `json:"attempts"`
	Truncated bool          `json:"truncated"`
}

// ErrNotTaskRow: the log exists but is not a native or image task row.
var ErrNotTaskRow = errors.New("log is not a native or image task row")

const (
	taskRequestsLimit = 100
	// taskRequestsStatementTimeout bounds each lookup. idx_logs_request_id
	// covers request_id only, so a key that hard-codes one ID on sync calls
	// makes the rejected-attempt lookup read those rows too (until step 3).
	taskRequestsStatementTimeout = 2 * time.Second
)

// GetTaskRequests lists the requests related to the task row logID of group:
//   - Q1a: rejected submits with the task's ID, group and key (FOLDABLE);
//   - Q1b: other rows carrying the task's billing operation, which also finds
//     failed submits of 1-7 character IDs.
//
// Each lookup is ordered by request_at, id, reads at most 100 rows and stops
// after 2 s; either limit sets Truncated. gorm.ErrRecordNotFound: no such row
// in the group; ErrNotTaskRow: the row is not a native or image task row.
func GetTaskRequests(
	ctx context.Context,
	group string,
	logID int,
	now time.Time,
) (*TaskRequests, error) {
	if group == "" {
		return nil, errors.New("group is required")
	}

	if now.IsZero() {
		now = time.Now()
	}

	db := LogDB.WithContext(ctx)

	var task Log
	if err := db.Where("id = ? AND group_id = ?", logID, group).First(&task).Error; err != nil {
		return nil, err
	}

	lane, taskID := task.taskLaneOf()
	if lane == taskLaneNone || taskID == "" {
		return nil, ErrNotTaskRow
	}

	// The same predicate as kind=tasks (admin base, so trials expand too).
	taskSQL, taskArgs := taskViewTaskExpr(true, now)

	var isTask int64
	if err := db.Model(&Log{}).
		Where("id = ?", task.ID).
		Where("("+taskSQL+")", taskArgs...).
		Count(&isTask).Error; err != nil {
		return nil, err
	}

	if isTask == 0 {
		return nil, ErrNotTaskRow
	}

	result := &TaskRequests{Task: &task, Attempts: []TaskRequest{}}

	foldSQL, foldArgs := foldableExpr(lane, now)

	rejected, truncated, err := readTaskRequests(ctx, func(tx *gorm.DB) *gorm.DB {
		return tx.Where("request_id = ? AND group_id = ?", taskID, group).
			Where(foldSQL, foldArgs...)
	})
	if err != nil {
		return nil, err
	}

	result.Truncated = result.Truncated || truncated

	for _, entry := range rejected {
		attempt := TaskRequest{Log: entry, Relation: TaskRequestRejected}
		if entry.RequestAt.Before(task.RequestAt) {
			attempt.Hint = TaskRequestHintBeforeSubmit
		}

		result.Attempts = append(result.Attempts, attempt)
	}

	if op := string(task.BillingOperationID); op != "" {
		failed, truncated, err := readTaskRequests(ctx, func(tx *gorm.DB) *gorm.DB {
			return tx.Where(
				"group_id = ? AND billing_operation_id = ? AND id <> ?",
				group,
				op,
				task.ID,
			)
		})
		if err != nil {
			return nil, err
		}

		result.Truncated = result.Truncated || truncated

		for _, entry := range failed {
			result.Attempts = append(
				result.Attempts,
				TaskRequest{Log: entry, Relation: TaskRequestFailedSubmit},
			)
		}
	}

	slices.SortStableFunc(result.Attempts, func(a, b TaskRequest) int {
		if c := a.Log.RequestAt.Compare(b.Log.RequestAt); c != 0 {
			return c
		}

		return a.Log.ID - b.Log.ID
	})

	task.SetCallStatus(now)

	for _, attempt := range result.Attempts {
		attempt.Log.SetCallStatus(now)
	}

	return result, nil
}

// readTaskRequests runs one bounded lookup. Its own deadline expiring (not the
// request's) marks the result truncated instead of failing it.
func readTaskRequests(ctx context.Context, scope func(*gorm.DB) *gorm.DB) ([]*Log, bool, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, taskRequestsStatementTimeout)
	defer cancel()

	var rows []*Log

	err := scope(LogDB.WithContext(lookupCtx).Model(&Log{})).
		Order("request_at asc, id asc").
		Limit(taskRequestsLimit + 1).
		Find(&rows).Error
	if err != nil {
		if ctx.Err() == nil && lookupCtx.Err() != nil {
			return nil, true, nil
		}

		return nil, false, err
	}

	if len(rows) > taskRequestsLimit {
		return rows[:taskRequestsLimit], true, nil
	}

	return rows, false, nil
}

// The columns the task view reads, and the task tables GetTaskRequests probes.
var (
	taskViewLogColumns = []string{
		"billing_operation_id",
		"async_usage_status",
		"endpoint",
		"error_code",
	}
	taskTableColumns    = []string{"id", "group_id", "token_id", "billing_operation_id"}
	taskViewReadyTables = []any{&NativeTask{}, &ImageTask{}}
)

func columnsSelectable(ctx context.Context, db *gorm.DB, table any, columns []string) error {
	quiet := db.WithContext(ctx).Session(&gorm.Session{Logger: logger.Discard, NewDB: true})

	rows, err := quiet.Model(table).Select(columns).Limit(0).Rows()
	if err != nil {
		return err
	}

	err = rows.Err()
	//nolint:sqlclosecheck // The LIMIT 0 schema probe must close before the next query on this transaction.
	_ = rows.Close()

	return err
}

// TaskViewStorageReady reports whether kind=tasks, kind=non_task and
// unit=tasks can run on db: the logs columns exist and, on Postgres, the
// usage attribution indexes (idx_logs_group_billing_op among them) exist and
// are valid. Without that index every SUPERSEDED probe would scan the group.
// An error means "not ready".
func TaskViewStorageReady(ctx context.Context, db *gorm.DB) (bool, error) {
	if db == nil {
		return false, nil
	}

	if err := columnsSelectable(ctx, db, &Log{}, taskViewLogColumns); err != nil {
		return false, err
	}

	if db.Name() != "postgres" {
		return true, nil
	}

	missing, err := MissingUsageIndexes(db.WithContext(ctx))
	if err != nil {
		return false, err
	}

	return len(missing) == 0, nil
}

// TaskRequestsStorageReady additionally needs the native_tasks and
// image_tasks tables that GetTaskRequests probes by primary key.
func TaskRequestsStorageReady(ctx context.Context, db *gorm.DB) (bool, error) {
	ready, err := TaskViewStorageReady(ctx, db)
	if !ready || err != nil {
		return false, err
	}

	for _, table := range taskViewReadyTables {
		if err := columnsSelectable(ctx, db, table, taskTableColumns); err != nil {
			return false, err
		}
	}

	return true, nil
}
