package model

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const taskViewGroup = "cust"

type taskViewRow struct {
	label string
	log   Log
}

// taskViewFixture is one customer's history with every case the task view
// must place, plus rows of other groups and of no group.
func taskViewFixture(now time.Time) []taskViewRow {
	at := func(age time.Duration) time.Time { return now.Add(-age) }
	native := func(id string) Log {
		return Log{
			RequestID: EmptyNullString(id), GroupID: taskViewGroup, TokenID: 7, TokenName: "prod",
			Model: "vendor/flux", Mode: int(mode.NativeTasks), Endpoint: nativeSubmitEndpoint,
		}
	}
	rejected := func(l Log, code int, stage FailureStage, errorCode string, age time.Duration) Log {
		l.Code, l.FailureStage, l.ErrorCode, l.RequestAt = code, stage, errorCode, at(age)
		return l
	}
	// A submit that failed after the hold: the row carries the task's op.
	failedAfterHold := func(l Log, op string, age time.Duration) Log {
		l.Code, l.FailureStage, l.ErrorCode = 503, FailureStageRouting, "native_execution_unavailable"
		l.BillingOperationID, l.RequestAt = EmptyNullString(op), at(age)
		return l
	}
	own := func(l Log, status AsyncUsageStatus, op string, age time.Duration) Log {
		l.Code, l.AsyncUsageStatus, l.BillingOperationID, l.RequestAt = 202, status, EmptyNullString(
			op,
		), at(
			age,
		)

		return l
	}
	chat := func(id string, code int, stage FailureStage, age time.Duration) Log {
		return Log{
			RequestID: EmptyNullString(
				id,
			),
			GroupID:   taskViewGroup,
			TokenID:   7,
			TokenName: "prod",
			Model:     "gpt-4o",
			Mode: int(
				mode.ChatCompletions,
			),
			Endpoint:     "/v1/chat/completions",
			Code:         code,
			FailureStage: stage,
			RequestAt:    at(age),
		}
	}

	a := native("job-a1234")
	b := native("job-b5678")
	b.TokenID, b.TokenName = 11, "batch"
	c := native("job-c9012")
	otherKey := a
	otherKey.TokenID, otherKey.TokenName = 8, "other-key"
	otherGroup := a
	otherGroup.GroupID, otherGroup.TokenID = "other", 5
	adminDemoChat := chat("demo-chat-1", 200, FailureStageNone, 19*time.Minute)
	adminDemoChat.RequestSource = RequestSourceAdminDemo
	internalDemoChat := adminDemoChat
	internalDemoChat.GroupID, internalDemoChat.TokenID, internalDemoChat.RequestID = "internal", 9, "demo-chat-2"

	return []taskViewRow{
		// One native task; rejected retries share its X-Request-Id.
		{
			"a-rejected-429-before",
			rejected(a, 429, FailureStageRateLimit, "too_many_active_tasks", 60*time.Minute),
		},
		{"a-own", own(a, AsyncUsageStatusCompleted, "native:job-a1234", 59*time.Minute)},
		{
			"a-rejected-429-replay",
			rejected(a, 429, FailureStageRateLimit, "rate_limit_exceeded", 58*time.Minute),
		},
		{
			"a-conflict-409",
			rejected(a, 409, FailureStageValidation, errorCodeRequestIDConflict, 57*time.Minute),
		},
		{
			"a-other-key-429",
			rejected(otherKey, 429, FailureStageRateLimit, "too_many_active_tasks", 57*time.Minute),
		},
		{
			"a-poll-404",
			Log{
				RequestID:    "job-a1234",
				GroupID:      taskViewGroup,
				TokenID:      7,
				Code:         404,
				FailureStage: FailureStageValidation,
				ErrorCode:    "task_not_found",
				Endpoint:     "GET /v1/model-tasks/job-a1234",
				RequestAt:    at(56 * time.Minute),
			},
		},
		{
			"k-other-group-429",
			rejected(
				otherGroup,
				429,
				FailureStageRateLimit,
				"too_many_active_tasks",
				55*time.Minute,
			),
		},
		// Failed after the hold, then resent with the same ID and accepted.
		{"b-failed-after-hold-503", failedAfterHold(b, "native:job-b5678", 50*time.Minute)},
		{"b-own", own(b, AsyncUsageStatusCompleted, "native:job-b5678", 49*time.Minute)},
		// Two failures and no own row: the earliest is the task.
		{"c-failed-first", failedAfterHold(c, "native:job-c9012", 45*time.Minute)},
		{"c-failed-second", failedAfterHold(c, "native:job-c9012", 44*time.Minute)},
		// A 1-7 character ID: the failed submit has a timestamp request ID.
		{
			"d-failed-short-id",
			failedAfterHold(native("1760000000000001"), "native:abc", 40*time.Minute),
		},
		{
			"d-own-processing",
			own(native("abc"), AsyncUsageStatusPending, "native:abc", 39*time.Minute),
		},
		// Image task: a gate refused a submit before the handler (the ID is
		// kept), the handler refused another (image-trace ID), a poll failed.
		{
			"e-image-gate-429-same-id",
			Log{
				RequestID:    "img-e1234",
				GroupID:      taskViewGroup,
				TokenID:      7,
				Endpoint:     imageSubmitEndpoint,
				Code:         429,
				FailureStage: FailureStageRateLimit,
				ErrorCode:    "rate_limited",
				RequestAt:    at(36 * time.Minute),
			},
		},
		{
			"e-image-own",
			Log{
				RequestID:          "img-e1234",
				GroupID:            taskViewGroup,
				TokenID:            7,
				Model:              "vendor/img",
				Mode:               int(mode.ImagesGenerations),
				Endpoint:           imageTaskRowEndpoint,
				Code:               202,
				AsyncUsageStatus:   AsyncUsageStatusCompleted,
				BillingOperationID: "image:img-e1234",
				RequestAt:          at(35 * time.Minute),
			},
		},
		{
			"e-image-trace-429",
			Log{
				RequestID:    "image-trace-1760000000000002",
				GroupID:      taskViewGroup,
				TokenID:      7,
				Endpoint:     imageSubmitEndpoint,
				Code:         429,
				FailureStage: FailureStageRateLimit,
				ErrorCode:    "too_many_active_tasks",
				RequestAt:    at(34 * time.Minute),
			},
		},
		{
			"e-image-poll-404",
			Log{
				RequestID:    "image-poll-1760000000000003",
				GroupID:      taskViewGroup,
				TokenID:      7,
				Code:         404,
				FailureStage: FailureStageRouting,
				ErrorCode:    "task_not_found",
				Endpoint:     "GET /v1/images/tasks/img-e1234",
				RequestAt:    at(33 * time.Minute),
			},
		},
		// A poll for an ID that was never a task.
		{
			"f-poll-unknown-404",
			Log{
				RequestID:    "poll-unknown-1",
				GroupID:      taskViewGroup,
				TokenID:      7,
				Code:         404,
				FailureStage: FailureStageValidation,
				ErrorCode:    "task_not_found",
				Endpoint:     "GET /v1/model-tasks/nope",
				RequestAt:    at(30 * time.Minute),
			},
		},
		// Sync calls: one row each, also when they reuse an X-Request-Id.
		{"g-chat-ok", chat("chat-1", 200, FailureStageNone, 29*time.Minute)},
		{"g-chat-upstream-502", chat("chat-2", 502, FailureStageUpstream, 28*time.Minute)},
		{"g-chat-rejected-402", chat("chat-3", 402, FailureStageBalance, 27*time.Minute)},
		{"g-chat-dup-first", chat("dup-1", 200, FailureStageNone, 26*time.Minute)},
		{"g-chat-dup-second", chat("dup-1", 200, FailureStageNone, 25*time.Minute)},
		{
			"h-video-processing",
			Log{
				RequestID:        "video-1",
				GroupID:          taskViewGroup,
				TokenID:          7,
				Model:            "sora-2",
				Mode:             int(mode.Videos),
				Endpoint:         "/v1/videos",
				Code:             200,
				AsyncUsageStatus: AsyncUsageStatusPending,
				RequestAt:        at(24 * time.Minute),
			},
		},
		{
			"m-video-poll-502",
			Log{
				RequestID:    "video-poll-1",
				GroupID:      taskViewGroup,
				TokenID:      7,
				Model:        "sora-2",
				Mode:         int(mode.VideosGet),
				Endpoint:     "/v1/videos/video_1",
				Code:         502,
				FailureStage: FailureStageUpstream,
				RequestAt:    at(23 * time.Minute),
			},
		},
		// Before key usage P2 a failure after the hold had no op: rejected.
		{
			"i-pre-p2-failed-503",
			rejected(
				native("job-i3456"),
				503,
				FailureStageRouting,
				"native_execution_unavailable",
				22*time.Minute,
			),
		},
		{"i-pre-p2-own", own(native("job-i3456"), AsyncUsageStatusCompleted, "", 21*time.Minute)},
		// Pending for more than a day.
		{
			"n-own-stale",
			own(native("job-n7890"), AsyncUsageStatusPending, "native:job-n7890", 25*time.Hour),
		},
		// The task exists, but no row carries its op (released hold, crash or
		// retention): its rejected attempts never fold.
		{
			"x-rejected-429-no-visible-task",
			rejected(
				native("job-x2468"),
				429,
				FailureStageRateLimit,
				"too_many_active_tasks",
				17*time.Minute,
			),
		},
		// Internal demos: admin only.
		{"cust-admin-demo-chat", adminDemoChat},
		{
			"j-trial",
			Log{
				RequestID:          "trial_j1234",
				GroupID:            "internal",
				TokenID:            9,
				TokenName:          "trial_ops",
				RequestSource:      RequestSourceAdminDemo,
				Model:              "vendor/flux",
				Mode:               int(mode.NativeTasks),
				Endpoint:           "POST /api/native-trials",
				Code:               202,
				AsyncUsageStatus:   AsyncUsageStatusCompleted,
				BillingOperationID: "native:trial_j1234",
				RequestAt:          at(20 * time.Minute),
			},
		},
		{"j-internal-demo-chat", internalDemoChat},
		{"j-unknown-key-401", Log{
			RequestID: "unknown-key-1", Code: 401, FailureStage: FailureStageAuth,
			Endpoint: "POST /v1/chat/completions", RequestAt: at(18 * time.Minute),
		}},
	}
}

var (
	wantCustomerTasks = []string{
		"a-own", "b-own", "c-failed-first", "d-own-processing", "e-image-own",
		"g-chat-dup-first", "g-chat-dup-second", "g-chat-ok", "g-chat-upstream-502",
		"h-video-processing", "i-pre-p2-own", "legacy-null-mode-endpoint", "n-own-stale",
	}
	wantCustomerNonTask = []string{
		"a-conflict-409",
		"a-other-key-429",
		"a-poll-404",
		"a-rejected-429-before",
		"a-rejected-429-replay",
		"b-failed-after-hold-503",
		"c-failed-second",
		"d-failed-short-id",
		"e-image-gate-429-same-id",
		"e-image-poll-404",
		"e-image-trace-429",
		"f-poll-unknown-404",
		"g-chat-rejected-402",
		"i-pre-p2-failed-503",
		"m-video-poll-502",
		"x-rejected-429-no-visible-task",
	}
	wantAdminOnlyTasks   = []string{"cust-admin-demo-chat", "j-internal-demo-chat", "j-trial"}
	wantAdminOnlyNonTask = []string{"j-unknown-key-401", "k-other-group-429"}
	wantSuperseded       = []string{
		"b-failed-after-hold-503",
		"c-failed-second",
		"d-failed-short-id",
	}
)

func sortedConcat(lists ...[]string) []string {
	out := []string{}
	for _, list := range lists {
		out = append(out, list...)
	}

	slices.Sort(out)

	return out
}

type taskViewSeed struct {
	labels map[int]string
	byID   map[int]*Log
	all    []*Log
	rows   map[string]*Log
}

func seedTaskViewRows(t *testing.T, db *gorm.DB, now time.Time) taskViewSeed {
	t.Helper()

	seed := taskViewSeed{labels: map[int]string{}, byID: map[int]*Log{}, rows: map[string]*Log{}}

	for _, row := range taskViewFixture(now) {
		entry := row.log
		entry.CreatedAt = entry.RequestAt.Add(time.Second)
		require.NoError(t, db.Create(&entry).Error, row.label)
		seed.labels[entry.ID] = row.label
	}

	// A row written before mode and endpoint were recorded is a sync call.
	require.NoError(t, db.Table("logs").Create(map[string]any{
		"group_id": taskViewGroup, "token_id": 7, "request_id": "legacy-1", "model": "legacy-model",
		"mode": nil, "endpoint": nil, "code": 200,
		"request_at": now.Add(-20 * time.Minute), "created_at": now.Add(-20 * time.Minute),
	}).Error)

	var stored []*Log
	require.NoError(t, db.Order("id").Find(&stored).Error)

	for _, entry := range stored {
		if entry.RequestID == "legacy-1" {
			seed.labels[entry.ID] = "legacy-null-mode-endpoint"
		}

		seed.byID[entry.ID] = entry
		seed.rows[seed.labels[entry.ID]] = entry
	}

	seed.all = stored

	return seed
}

func (s taskViewSeed) labelsOf(t *testing.T, logs []*Log) []string {
	t.Helper()

	out := make([]string, 0, len(logs))
	for _, entry := range logs {
		label, ok := s.labels[entry.ID]
		require.True(t, ok, "unexpected log %d", entry.ID)

		out = append(out, label)
	}

	slices.Sort(out)

	return out
}

// goLabels classifies every seeded row with the Go forms.
func (s taskViewSeed) goLabels(keep func(*Log) bool) []string {
	out := []string{}
	for _, entry := range s.all {
		if keep(entry) {
			out = append(out, s.labels[entry.ID])
		}
	}

	slices.Sort(out)

	return out
}

func withTaskViewDBs(t *testing.T, fn func(t *testing.T, db *gorm.DB)) {
	t.Helper()

	t.Run("sqlite", func(t *testing.T) {
		db, err := OpenSQLite(filepath.Join(t.TempDir(), "task-view.db"))
		require.NoError(t, err)

		previous := LogDB
		LogDB = db
		t.Cleanup(func() {
			LogDB = previous

			if sqlDB, err := db.DB(); err == nil {
				_ = sqlDB.Close()
			}
		})

		require.NoError(
			t,
			db.AutoMigrate(
				&Log{},
				&RequestDetail{},
				&Summary{},
				&GroupSummary{},
				&NativeTask{},
				&ImageTask{},
			),
		)
		fn(t, db)
	})

	t.Run("postgres", func(t *testing.T) {
		withTestPostgresStoreDB(t, func() {
			require.NoError(
				t,
				LogDB.AutoMigrate(
					&Log{},
					&RequestDetail{},
					&Summary{},
					&GroupSummary{},
					&NativeTask{},
					&ImageTask{},
				),
			)
			fn(t, LogDB)
		})
	})
}

func TestTaskViewKinds(t *testing.T) {
	withTaskViewDBs(t, assertTaskViewKinds)
}

func assertTaskViewKinds(t *testing.T, db *gorm.DB) {
	t.Helper()

	now := time.Now()
	seed := seedTaskViewRows(t, db, now)
	start, end := now.Add(-48*time.Hour), now.Add(time.Hour)

	// The hand-written expectation and the Go forms agree.
	customerTask := func(l *Log) bool { return l.GroupID == taskViewGroup && l.isTaskViewTask(false, seed.all) }
	customerNonTask := func(l *Log) bool {
		return l.GroupID == taskViewGroup && l.RequestSource != RequestSourceAdminDemo &&
			!l.isTaskViewTask(false, seed.all)
	}
	adminTask := func(l *Log) bool { return l.isTaskViewTask(true, seed.all) }

	require.Equal(t, wantCustomerTasks, seed.goLabels(customerTask))
	require.Equal(t, wantCustomerNonTask, seed.goLabels(customerNonTask))
	require.Equal(t, sortedConcat(wantCustomerTasks, wantAdminOnlyTasks), seed.goLabels(adminTask))
	require.Equal(
		t,
		wantSuperseded,
		seed.goLabels(func(l *Log) bool { return l.supersededAmong(seed.all) }),
	)

	groupLogs := func(kind GroupLogKind, status CallStatus, requestID string, page, perPage int) *GetGroupLogsResult {
		t.Helper()

		result, err := GetGroupLogs(context.Background(), GroupLogQuery{
			Group:     taskViewGroup,
			Windowed:  true,
			Start:     start,
			End:       end,
			Kind:      kind,
			Status:    status,
			RequestID: requestID,
			Page:      page,
			PerPage:   perPage,
			Now:       now,
		})
		require.NoError(t, err)

		return result
	}

	// Customer: tasks and non_task partition the visible rows.
	tasks := groupLogs(GroupLogKindTasks, "", "", 1, 100)
	require.Equal(t, wantCustomerTasks, seed.labelsOf(t, tasks.Logs))
	require.EqualValues(t, len(wantCustomerTasks), tasks.Total)

	nonTask := groupLogs(GroupLogKindNonTask, "", "", 1, 100)
	require.Equal(t, wantCustomerNonTask, seed.labelsOf(t, nonTask.Logs))
	require.EqualValues(t, len(wantCustomerNonTask), nonTask.Total)

	all := groupLogs(GroupLogKindAll, "", "", 1, 100)
	visible := []*Log{}

	for _, entry := range all.Logs {
		if entry.RequestSource != RequestSourceAdminDemo {
			visible = append(visible, entry)
		}
	}

	require.Equal(
		t,
		sortedConcat(wantCustomerTasks, wantCustomerNonTask),
		seed.labelsOf(t, visible),
	)

	// One native task with rejected retries under the same X-Request-Id is
	// one task row; its attempts and its failed poll are in non_task.
	require.Equal(
		t,
		[]string{"a-own"},
		seed.labelsOf(t, groupLogs(GroupLogKindTasks, "", "job-a1234", 1, 100).Logs),
	)
	require.Equal(
		t,
		[]string{
			"a-conflict-409",
			"a-other-key-429",
			"a-poll-404",
			"a-rejected-429-before",
			"a-rejected-429-replay",
		},
		seed.labelsOf(t, groupLogs(GroupLogKindNonTask, "", "job-a1234", 1, 100).Logs),
	)

	// Status filters the task rows.
	for _, status := range []CallStatus{CallStatusSucceeded, CallStatusFailed, CallStatusProcessing, CallStatusUnknown} {
		want := seed.goLabels(
			func(l *Log) bool { return customerTask(l) && l.CallStatus(now) == status },
		)
		require.NotEmpty(t, want, status)

		result := groupLogs(GroupLogKindTasks, status, "", 1, 100)
		require.Equal(t, want, seed.labelsOf(t, result.Logs), status)
		require.EqualValues(t, len(want), result.Total, status)
	}

	// Totals equal the rows across pages, with no row on two pages.
	for _, kind := range []GroupLogKind{GroupLogKindTasks, GroupLogKindNonTask} {
		var (
			paged []*Log
			total int64
		)

		for page := 1; ; page++ {
			result := groupLogs(kind, "", "", page, 3)
			total = result.Total

			if len(result.Logs) == 0 {
				break
			}

			paged = append(paged, result.Logs...)
		}

		require.Len(t, paged, int(total), kind)
		require.Equal(
			t,
			seed.labelsOf(t, groupLogs(kind, "", "", 1, 100).Logs),
			seed.labelsOf(t, paged),
			kind,
		)
	}

	// Admin: every row, trials and demos among tasks, unidentified callers
	// and other groups among non_task.
	adminLogs := func(kind AdminLogKind, group string) *GetLogsResult {
		t.Helper()

		filter := OperationalLogFilter{Kind: kind, Now: now}
		if group == "" {
			result, err := GetLogs(
				start,
				end,
				"",
				"",
				"",
				0,
				filter,
				"",
				CodeTypeAll,
				0,
				false,
				"",
				"",
				1,
				100,
			)
			require.NoError(t, err)

			return result
		}

		result, err := SearchLogs(
			"",
			"",
			"",
			group,
			0,
			"",
			"",
			start,
			end,
			0,
			filter,
			"",
			CodeTypeAll,
			0,
			false,
			"",
			"",
			1,
			100,
		)
		require.NoError(t, err)

		return result
	}

	wantAdminTasks := sortedConcat(wantCustomerTasks, wantAdminOnlyTasks)
	wantAdminNonTask := sortedConcat(wantCustomerNonTask, wantAdminOnlyNonTask)

	result := adminLogs(AdminLogKindTasks, "")
	require.Equal(t, wantAdminTasks, seed.labelsOf(t, result.Logs))
	require.EqualValues(t, len(wantAdminTasks), result.Total)

	result = adminLogs(AdminLogKindNonTask, "")
	require.Equal(t, wantAdminNonTask, seed.labelsOf(t, result.Logs))
	require.EqualValues(t, len(wantAdminNonTask), result.Total)
	require.Len(
		t,
		seed.all,
		len(wantAdminTasks)+len(wantAdminNonTask),
		"admin tasks and non_task cover every row",
	)

	result = adminLogs(AdminLogKindTasks, taskViewGroup)
	require.Equal(
		t,
		sortedConcat(wantCustomerTasks, []string{"cust-admin-demo-chat"}),
		seed.labelsOf(t, result.Logs),
	)

	result = adminLogs(AdminLogKindNonTask, taskViewGroup)
	require.Equal(t, wantCustomerNonTask, seed.labelsOf(t, result.Logs))

	// Existing kinds are unchanged.
	require.EqualValues(t,
		len(seed.goLabels(func(l *Log) bool { return l.GroupID == taskViewGroup && l.IsCall() })),
		groupLogs(GroupLogKindCalls, "", "", 1, 100).Total,
	)
	require.EqualValues(t,
		len(seed.goLabels(func(l *Log) bool { return l.isAdminLogCall() })),
		adminLogs(AdminLogKindCalls, "").Total,
	)

	// unit=tasks: superseded rows drop out of the counts; nothing else moves.
	requests, err := GetGroupCallTotals(
		context.Background(),
		taskViewGroup,
		start,
		end,
		now,
		CallUnitRequests,
	)
	require.NoError(t, err)
	tasksTotals, err := GetGroupCallTotals(
		context.Background(),
		taskViewGroup,
		start,
		end,
		now,
		CallUnitTasks,
	)
	require.NoError(t, err)

	require.Equal(
		t,
		requests.Totals.Requests-int64(len(wantSuperseded)),
		tasksTotals.Totals.Requests,
	)
	require.Equal(t, requests.Totals.Failed-int64(len(wantSuperseded)), tasksTotals.Totals.Failed)
	require.Equal(t, requests.Totals.Succeeded, tasksTotals.Totals.Succeeded)
	require.Equal(t, requests.Totals.Rejected, tasksTotals.Totals.Rejected)
	require.Equal(t, requests.Totals.Processing, tasksTotals.Totals.Processing)
	require.Equal(t, requests.Totals.Unknown, tasksTotals.Totals.Unknown)

	// The task count equals the task list's total over the same window.
	unrejected := tasksTotals.Totals.Requests - tasksTotals.Totals.Rejected
	require.EqualValues(t, tasks.Total, unrejected)

	// Counts per key: the failed-then-resent submit of key 11 is one task.
	byRequest, err := GetGroupKeyCallStats(
		context.Background(),
		taskViewGroup,
		[]int{11, 7},
		start,
		end,
		now,
		CallUnitRequests,
	)
	require.NoError(t, err)
	byTask, err := GetGroupKeyCallStats(
		context.Background(),
		taskViewGroup,
		[]int{11, 7},
		start,
		end,
		now,
		CallUnitTasks,
	)
	require.NoError(t, err)

	require.EqualValues(t, 2, byRequest[0].Requests)
	require.EqualValues(t, 1, byRequest[0].Failed)
	require.EqualValues(t, 1, byRequest[0].Succeeded)
	require.EqualValues(t, 1, byTask[0].Requests)
	require.EqualValues(t, 0, byTask[0].Failed)
	require.EqualValues(t, 1, byTask[0].Succeeded)
	require.Equal(t, byRequest[0].LastRequestAt, byTask[0].LastRequestAt, "last used stays per row")
	require.Equal(
		t,
		byRequest[1].Requests-2,
		byTask[1].Requests,
		"key 7 loses c-failed-second and d-failed-short-id",
	)
	require.Equal(t, byRequest[1].Rejected, byTask[1].Rejected)
}

// TestTaskViewSQLMatchesGo checks SUPERSEDED and TASK in SQL against their Go
// forms over every combination of the columns they read, NULLs included.
func TestTaskViewSQLMatchesGo(t *testing.T) {
	withTaskViewDBs(t, assertTaskViewSQLMatchesGo)
}

func assertTaskViewSQLMatchesGo(t *testing.T, db *gorm.DB) {
	t.Helper()

	now := time.Now()
	rows := make([]Log, 0, 3840)

	for _, group := range []string{"g", "h"} {
		for _, endpoint := range []EmptyNullString{nativeSubmitEndpoint, imageSubmitEndpoint, imageTaskRowEndpoint, "", "GET /v1/model-tasks/x"} {
			for _, async := range []AsyncUsageStatus{AsyncUsageStatusNone, AsyncUsageStatusPending, AsyncUsageStatusCompleted, AsyncUsageStatusFailed} {
				for _, op := range []EmptyNullString{"", "native:o1", "native:o2"} {
					for _, code := range []int{200, 202, 429, 503} {
						for _, stage := range []FailureStage{FailureStageNone, FailureStageRouting} {
							for _, source := range []string{RequestSourceAPI, RequestSourceAdminDemo} {
								for _, tokenID := range []int{0, 7} {
									rows = append(rows, Log{
										GroupID:            group,
										TokenID:            tokenID,
										Code:               code,
										AsyncUsageStatus:   async,
										FailureStage:       stage,
										Mode:               int(mode.NativeTasks),
										Endpoint:           endpoint,
										RequestSource:      source,
										BillingOperationID: op,
										RequestAt: now.Add(
											-time.Hour,
										),
										CreatedAt: now.Add(-time.Hour),
									})
								}
							}
						}
					}
				}
			}
		}
	}

	require.NoError(t, db.CreateInBatches(&rows, 300).Error)

	// Historical NULLs classify like zero values. An op never has a NULL
	// group: the hold needs one.
	for _, column := range []string{"endpoint", "async_usage_status", "billing_operation_id", "request_source", "mode", "token_id", "group_id"} {
		values := map[string]any{
			"group_id":           "g",
			"token_id":           7,
			"code":               503,
			"async_usage_status": 0,
			"mode": int(
				mode.NativeTasks,
			),
			"endpoint":             nativeSubmitEndpoint,
			"request_source":       RequestSourceAPI,
			"failure_stage":        string(FailureStageRouting),
			"billing_operation_id": "native:o1",
			"request_at":           now,
			"created_at":           now,
		}
		if column == "group_id" {
			values["billing_operation_id"] = "native:null-group"
		}

		values[column] = nil
		require.NoError(t, db.Table("logs").Create(values).Error, column)
	}

	customerSQL, customerArgs := taskViewTaskExpr(false, now)
	adminSQL, adminArgs := taskViewTaskExpr(true, now)
	supersededSQL, supersededArgs := supersededExpr()
	nonTaskSQL, nonTaskArgs := taskViewNonTaskExpr(false, now)

	args := make([]any, 0, len(customerArgs)+len(adminArgs)+len(supersededArgs)+len(nonTaskArgs))
	args = append(args, customerArgs...)
	args = append(args, adminArgs...)
	args = append(args, supersededArgs...)
	args = append(args, nonTaskArgs...)

	var classified []struct {
		ID          int
		TaskCust    int
		TaskAdmin   int
		Superseded  int
		NonTaskCust int
	}
	require.NoError(t, db.Model(&Log{}).Select(
		"id, CASE WHEN "+customerSQL+" THEN 1 ELSE 0 END AS task_cust,"+
			" CASE WHEN "+adminSQL+" THEN 1 ELSE 0 END AS task_admin,"+
			" CASE WHEN "+supersededSQL+" THEN 1 ELSE 0 END AS superseded,"+
			" CASE WHEN "+nonTaskSQL+" THEN 1 ELSE 0 END AS non_task_cust",
		args...,
	).Find(&classified).Error)

	var stored []*Log
	require.NoError(t, db.Find(&stored).Error)
	require.Len(t, classified, len(stored))

	byID := make(map[int]*Log, len(stored))
	for _, entry := range stored {
		byID[entry.ID] = entry
	}

	counts := map[string]int{}

	for _, row := range classified {
		entry := byID[row.ID]
		require.NotNil(t, entry)

		description := fmt.Sprintf("%+v", *entry)
		require.Equal(t, entry.supersededAmong(stored), row.Superseded == 1, description)
		require.Equal(t, entry.isTaskViewTask(false, stored), row.TaskCust == 1, description)
		require.Equal(t, entry.isTaskViewTask(true, stored), row.TaskAdmin == 1, description)
		require.Equal(t,
			entry.RequestSource != RequestSourceAdminDemo && !entry.isTaskViewTask(false, stored),
			row.NonTaskCust == 1, description)

		counts["superseded"] += row.Superseded
		counts["task_cust"] += row.TaskCust
		counts["task_admin"] += row.TaskAdmin
	}

	for _, name := range []string{"superseded", "task_cust", "task_admin"} {
		require.Positive(t, counts[name], "fixture covers %s", name)
	}

	// Neither expression is ever NULL: TASK and NOT TASK split every row.
	for _, admin := range []bool{false, true} {
		taskSQL, taskArgs := taskViewTaskExpr(admin, now)

		var inTask, notTask int64
		require.NoError(
			t,
			db.Model(&Log{}).Where("("+taskSQL+")", taskArgs...).Count(&inTask).Error,
		)
		require.NoError(
			t,
			db.Model(&Log{}).Where("NOT ("+taskSQL+")", taskArgs...).Count(&notTask).Error,
		)
		require.EqualValues(t, len(stored), inTask+notTask, "admin=%v", admin)
	}
}

// foldableAmong is the Go form of foldableExpr: tasks are the lane's task
// table, peers every log row.
func (l *Log) foldableAmong(lane taskLane, tasks []taskRef, peers []*Log) bool {
	endpoint := nativeSubmitEndpoint
	if lane == taskLaneImage {
		endpoint = imageSubmitEndpoint
	}

	if string(l.Endpoint) != endpoint || l.AsyncUsageStatus != AsyncUsageStatusNone ||
		l.CallStatus(
			time.Now(),
		) != CallStatusRejected || l.ErrorCode == errorCodeRequestIDConflict {
		return false
	}

	for _, task := range tasks {
		if task.ID != string(l.RequestID) || task.GroupID != l.GroupID ||
			task.TokenID != l.TokenID {
			continue
		}

		for _, peer := range peers {
			if peer.GroupID == task.GroupID && task.BillingOperationID != "" &&
				string(peer.BillingOperationID) == task.BillingOperationID {
				return true
			}
		}
	}

	return false
}

type taskRef struct {
	ID                 string
	GroupID            string
	TokenID            int
	BillingOperationID string
}

func seedTaskTables(t *testing.T, db *gorm.DB) ([]taskRef, []taskRef) {
	t.Helper()

	natives := []taskRef{
		{"job-a1234", taskViewGroup, 7, "native:job-a1234"},
		{"job-b5678", taskViewGroup, 11, "native:job-b5678"},
		{"job-c9012", taskViewGroup, 7, "native:job-c9012"},
		{"abc", taskViewGroup, 7, "native:abc"},
		{"job-i3456", taskViewGroup, 7, ""},
		{"job-n7890", taskViewGroup, 7, "native:job-n7890"},
		{"job-x2468", taskViewGroup, 7, "native:job-x2468"},
		{"trial_j1234", "internal", 9, "native:trial_j1234"},
	}
	for _, ref := range natives {
		require.NoError(t, db.Create(&NativeTask{
			ID:                 ref.ID,
			GroupID:            ref.GroupID,
			TokenID:            ref.TokenID,
			BillingOperationID: ref.BillingOperationID,
			Model:              "vendor/flux",
			Status:             "completed",
		}).Error, ref.ID)
	}

	images := []taskRef{{"img-e1234", taskViewGroup, 7, "image:img-e1234"}}
	for _, ref := range images {
		require.NoError(t, db.Create(&ImageTask{
			ID:                 ref.ID,
			GroupID:            ref.GroupID,
			TokenID:            ref.TokenID,
			BillingOperationID: ref.BillingOperationID,
			Model:              "vendor/img",
			Status:             "succeeded",
		}).Error, ref.ID)
	}

	return natives, images
}

func TestTaskRequests(t *testing.T) {
	withTaskViewDBs(t, assertTaskRequests)
}

func assertTaskRequests(t *testing.T, db *gorm.DB) {
	t.Helper()

	now := time.Now()
	seed := seedTaskViewRows(t, db, now)
	natives, images := seedTaskTables(t, db)

	// FOLDABLE in SQL and Go: only same group, same key, not a 409, and only
	// when the task has a row carrying its op.
	for _, lane := range []taskLane{taskLaneNative, taskLaneImage} {
		tasks := natives
		if lane == taskLaneImage {
			tasks = images
		}

		foldSQL, foldArgs := foldableExpr(lane, now)

		var folded []*Log
		require.NoError(t, db.Model(&Log{}).Where(foldSQL, foldArgs...).Find(&folded).Error)
		require.Equal(t,
			seed.goLabels(func(l *Log) bool { return l.foldableAmong(lane, tasks, seed.all) }),
			seed.labelsOf(t, folded), "lane %d", lane)
	}

	var folded []*Log

	foldSQL, foldArgs := foldableExpr(taskLaneNative, now)
	require.NoError(t, db.Model(&Log{}).Where(foldSQL, foldArgs...).Find(&folded).Error)
	require.Equal(
		t,
		[]string{"a-rejected-429-before", "a-rejected-429-replay"},
		seed.labelsOf(t, folded),
	)

	related := func(label string) *TaskRequests {
		t.Helper()

		result, err := GetTaskRequests(
			context.Background(),
			seed.rows[label].GroupID,
			seed.rows[label].ID,
			now,
		)
		require.NoError(t, err, label)
		require.Equal(t, seed.rows[label].ID, result.Task.ID)

		return result
	}

	type attempt struct {
		label    string
		relation TaskRequestRelation
		hint     string
	}

	attemptsOf := func(result *TaskRequests) []attempt {
		out := make([]attempt, 0, len(result.Attempts))
		for _, entry := range result.Attempts {
			out = append(out, attempt{seed.labels[entry.Log.ID], entry.Relation, entry.Hint})
		}

		return out
	}

	// Rejected attempts under the task's ID, in time order; the 409, the
	// other key, the other group and the failed poll are not the task's.
	result := related("a-own")
	require.False(t, result.Truncated)
	require.Equal(t, []attempt{
		{"a-rejected-429-before", TaskRequestRejected, TaskRequestHintBeforeSubmit},
		{"a-rejected-429-replay", TaskRequestRejected, ""},
	}, attemptsOf(result))

	// The submit that failed after the hold, found by op.
	require.Equal(
		t,
		[]attempt{{"b-failed-after-hold-503", TaskRequestFailedSubmit, ""}},
		attemptsOf(related("b-own")),
	)
	require.Equal(
		t,
		[]attempt{{"c-failed-second", TaskRequestFailedSubmit, ""}},
		attemptsOf(related("c-failed-first")),
	)
	// A 1-7 character ID: the failed submit's request ID is a timestamp.
	require.Equal(
		t,
		[]attempt{{"d-failed-short-id", TaskRequestFailedSubmit, ""}},
		attemptsOf(related("d-own-processing")),
	)
	// Image: a gate rejection that kept the ID folds; image-trace rows never.
	require.Equal(t,
		[]attempt{{"e-image-gate-429-same-id", TaskRequestRejected, TaskRequestHintBeforeSubmit}},
		attemptsOf(related("e-image-own")))
	// Before P2: no op on the task, so nothing folds.
	require.Empty(t, related("i-pre-p2-own").Attempts)
	require.Empty(t, related("j-trial").Attempts)

	for _, label := range []string{"g-chat-ok", "a-rejected-429-before", "b-failed-after-hold-503", "h-video-processing", "a-poll-404"} {
		_, err := GetTaskRequests(
			context.Background(),
			seed.rows[label].GroupID,
			seed.rows[label].ID,
			now,
		)
		require.ErrorIs(t, err, ErrNotTaskRow, label)
	}

	// Another group never sees the row.
	_, err := GetTaskRequests(context.Background(), "other", seed.rows["a-own"].ID, now)
	require.True(t, errors.Is(err, gorm.ErrRecordNotFound), "%v", err)

	// 300 rows sharing one op: one task (the earliest), and the lookup stops
	// at 100 rows and says so.
	const many = 300

	manyRows := make([]Log, 0, many)
	for index := range many {
		requestAt := now.Add(-2*time.Hour + time.Duration(index)*time.Second)
		manyRows = append(manyRows, Log{
			RequestID: "job-many", GroupID: "many", TokenID: 3, Mode: int(mode.NativeTasks),
			Endpoint: nativeSubmitEndpoint, Code: 503, FailureStage: FailureStageRouting,
			BillingOperationID: "native:job-many", RequestAt: requestAt, CreatedAt: requestAt,
		})
	}

	require.NoError(t, db.CreateInBatches(&manyRows, 100).Error)

	manyTasks, err := GetGroupLogs(context.Background(), GroupLogQuery{
		Group:    "many",
		Windowed: true,
		Start:    now.Add(-3 * time.Hour),
		End:      now,
		Kind:     GroupLogKindTasks,
		Page:     1,
		PerPage:  100,
		Now:      now,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, manyTasks.Total)
	require.Len(t, manyTasks.Logs, 1)
	require.Equal(t, manyRows[0].ID, manyTasks.Logs[0].ID)

	manyNonTask, err := GetGroupLogs(context.Background(), GroupLogQuery{
		Group:    "many",
		Windowed: true,
		Start:    now.Add(-3 * time.Hour),
		End:      now,
		Kind:     GroupLogKindNonTask,
		Page:     1,
		PerPage:  10,
		Now:      now,
	})
	require.NoError(t, err)
	require.EqualValues(t, many-1, manyNonTask.Total)

	manyRelated, err := GetTaskRequests(context.Background(), "many", manyRows[0].ID, now)
	require.NoError(t, err)
	require.True(t, manyRelated.Truncated)
	require.Len(t, manyRelated.Attempts, taskRequestsLimit)
	require.Equal(t, manyRows[1].ID, manyRelated.Attempts[0].Log.ID)

	// Many rejected attempts under one ID are cut at the same limit.
	rejectedRows := make([]Log, 0, taskRequestsLimit+5)
	for index := range taskRequestsLimit + 5 {
		requestAt := now.Add(-time.Hour + time.Duration(index)*time.Second)
		rejectedRows = append(rejectedRows, Log{
			RequestID:    "job-a1234",
			GroupID:      taskViewGroup,
			TokenID:      7,
			Mode:         int(mode.NativeTasks),
			Endpoint:     nativeSubmitEndpoint,
			Code:         429,
			FailureStage: FailureStageRateLimit,
			ErrorCode: "rate_limit_exceeded" + strconv.Itoa(
				index%2,
			),
			RequestAt: requestAt,
			CreatedAt: requestAt,
		})
	}

	require.NoError(t, db.CreateInBatches(&rejectedRows, 100).Error)

	result = related("a-own")
	require.True(t, result.Truncated)
	require.Len(t, result.Attempts, taskRequestsLimit)
}

func TestTaskViewStorageReady(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		db, err := OpenSQLite(filepath.Join(t.TempDir(), "ready.db"))
		require.NoError(t, err)
		t.Cleanup(func() {
			if sqlDB, err := db.DB(); err == nil {
				_ = sqlDB.Close()
			}
		})

		ctx := context.Background()

		ready, err := TaskViewStorageReady(ctx, db)
		require.False(t, ready)
		require.Error(t, err, "no logs table")

		require.NoError(t, db.AutoMigrate(&Log{}))

		ready, err = TaskViewStorageReady(ctx, db)
		require.NoError(t, err)
		require.True(t, ready)

		ready, err = TaskRequestsStorageReady(ctx, db)
		require.False(t, ready)
		require.Error(t, err, "no task tables")

		require.NoError(t, db.AutoMigrate(&NativeTask{}, &ImageTask{}))

		ready, err = TaskRequestsStorageReady(ctx, db)
		require.NoError(t, err)
		require.True(t, ready)

		// A LogDB before key usage P2 lacks the op column.
		require.NoError(t, db.Exec("ALTER TABLE logs DROP COLUMN billing_operation_id").Error)

		ready, err = TaskViewStorageReady(ctx, db)
		require.False(t, ready)
		require.Error(t, err)
	})

	t.Run("postgres", func(t *testing.T) {
		withTestPostgresStoreDB(t, func() {
			ctx := context.Background()

			require.NoError(t, LogDB.AutoMigrate(&Log{}))

			// Postgres gets the usage attribution indexes only from the
			// reviewed SQL: without them the view is not ready.
			ready, err := TaskViewStorageReady(ctx, LogDB)
			require.NoError(t, err)
			require.False(t, ready)

			for _, statement := range usageAttributionSQLiteIndexes {
				require.NoError(t, LogDB.Exec(statement).Error, statement)
			}

			ready, err = TaskViewStorageReady(ctx, LogDB)
			require.NoError(t, err)
			require.True(t, ready)
		})
	})
}
