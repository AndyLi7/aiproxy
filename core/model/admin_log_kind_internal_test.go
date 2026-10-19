package model

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// The admin request log keeps trial and admin demo rows among calls, unlike
// the customer list (TestIsCall).
func TestIsAdminLogCall(t *testing.T) {
	call := Log{
		GroupID:  "g",
		TokenID:  7,
		Mode:     int(mode.ChatCompletions),
		Endpoint: "/v1/chat/completions",
	}
	require.True(t, call.isAdminLogCall())

	for _, tc := range []struct {
		name   string
		mutate func(*Log)
		want   bool
	}{
		{"native task submit", func(l *Log) { l.Mode, l.Endpoint = int(mode.NativeTasks), "POST /v1/model-tasks" }, true},
		{"image task submit", func(l *Log) { l.Mode, l.Endpoint = int(mode.ImagesGenerations), "/v1/images/tasks" }, true},
		{"rejected submit", func(l *Log) { l.Mode, l.Endpoint, l.Code = int(mode.NativeTasks), "POST /v1/model-tasks", 429 }, true},
		{"admin demo", func(l *Log) { l.RequestSource = RequestSourceAdminDemo }, true},
		{"native trial", func(l *Log) {
			l.RequestSource, l.Mode, l.Endpoint = RequestSourceAdminDemo, int(mode.NativeTasks), "POST /api/native-trials"
		}, true},
		{"historical unknown mode", func(l *Log) { l.Mode = 999 }, true},
		{"key not recognised", func(l *Log) { l.TokenID = 0 }, false},
		{"no group", func(l *Log) { l.GroupID = "" }, false},
		{"relay read mode", func(l *Log) { l.Mode, l.Code = int(mode.VideosGet), 502 }, false},
		{"failed native poll", func(l *Log) { l.Mode, l.Endpoint, l.Code = 0, "GET /v1/model-tasks/x", 404 }, false},
		{"failed image poll", func(l *Log) { l.Mode, l.Endpoint, l.Code = 0, "GET /v1/images/tasks/x", 404 }, false},
		{"failed admin demo poll", func(l *Log) {
			l.RequestSource, l.Endpoint, l.Code = RequestSourceAdminDemo, "GET /v1/images/tasks/x", 404
		}, false},
		{"failed delete", func(l *Log) { l.Mode, l.Endpoint, l.Code = 0, "DELETE /v1/videos/x", 404 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := call
			tc.mutate(&entry)
			require.Equal(t, tc.want, entry.isAdminLogCall())
		})
	}
}

// adminLogKindRow is one request the admin log must place; read says whether
// kind=reads lists it.
type adminLogKindRow struct {
	label string
	log   Log
	read  bool
}

func adminLogKindRows(now time.Time) []adminLogKindRow {
	at := func(age time.Duration) time.Time { return now.Add(-age) }

	return []adminLogKindRow{
		// One generation, task ID job-7f3a91: a rejected attempt, the task row
		// and a poll sent with the same X-Request-Id before the submit.
		{label: "native-poll-404-before-submit", read: true, log: Log{
			RequestID: "job-7f3a91", GroupID: "cust", TokenID: 7, TokenName: "prod", Code: 404,
			FailureStage: FailureStageValidation, ErrorCode: "task_not_found",
			Endpoint: "GET /v1/model-tasks/job-7f3a91", RequestAt: at(10 * time.Minute),
		}},
		{label: "native-submit-rejected-429", log: Log{
			RequestID: "job-7f3a91", GroupID: "cust", TokenID: 7, TokenName: "prod", Code: 429,
			FailureStage: FailureStageRateLimit, Model: "vendor/flux", Mode: int(mode.NativeTasks),
			Endpoint: "POST /v1/model-tasks", RequestAt: at(9 * time.Minute),
		}},
		{label: "native-submit", log: Log{
			RequestID:          "job-7f3a91",
			GroupID:            "cust",
			TokenID:            7,
			TokenName:          "prod",
			Code:               202,
			AsyncUsageStatus:   AsyncUsageStatusCompleted,
			Model:              "vendor/flux",
			Mode:               int(mode.NativeTasks),
			Endpoint:           "POST /v1/model-tasks",
			BillingOperationID: "native:job-7f3a91",
			RequestAt:          at(8 * time.Minute),
		}},
		{label: "native-artifact-download-502", read: true, log: Log{
			RequestID:    "artifact-req-1",
			GroupID:      "cust",
			TokenID:      7,
			TokenName:    "prod",
			Code:         502,
			FailureStage: FailureStageUpstream,
			Endpoint:     "GET /v1/model-tasks/job-7f3a91/artifacts/0",
			RequestAt:    at(7 * time.Minute),
		}},
		// An image task poll replaces its request ID (controller/image_task.go).
		{label: "image-poll-404", read: true, log: Log{
			RequestID:    "image-poll-1760400000000",
			GroupID:      "cust",
			TokenID:      7,
			TokenName:    "prod",
			Code:         404,
			FailureStage: FailureStageRouting,
			ErrorCode:    "task_not_found",
			Endpoint:     "GET /v1/images/tasks/img-1",
			RequestAt:    at(6 * time.Minute),
		}},
		{label: "video-poll-relay-502", read: true, log: Log{
			RequestID: "video-poll-1", GroupID: "cust", TokenID: 7, TokenName: "prod", Code: 502,
			FailureStage: FailureStageUpstream, Model: "sora-2", Mode: int(mode.VideosGet),
			Endpoint: "/v1/videos/video_123", RequestAt: at(5 * time.Minute),
		}},
		{label: "chat-succeeded", log: Log{
			RequestID: "chat-1", GroupID: "cust", TokenID: 7, TokenName: "prod", Code: 200,
			Model: "gpt-4o", Mode: int(mode.ChatCompletions), Endpoint: "/v1/chat/completions",
			RequestAt: at(4 * time.Minute),
		}},
		// TokenAuth identified the key and refused it: the key's own call.
		{label: "disabled-key-403", log: Log{
			RequestID:    "disabled-key-1",
			GroupID:      "cust",
			TokenID:      8,
			TokenName:    "old",
			Code:         403,
			FailureStage: FailureStageAuth,
			Endpoint:     "POST /v1/chat/completions",
			RequestAt:    at(3 * time.Minute),
		}},
		// Trials and admin demos are calls in the admin log.
		{label: "native-trial", log: Log{
			RequestID:          "trial_abc",
			GroupID:            "internal",
			TokenID:            9,
			TokenName:          "trial_ops",
			Code:               202,
			AsyncUsageStatus:   AsyncUsageStatusCompleted,
			RequestSource:      RequestSourceAdminDemo,
			Model:              "vendor/flux",
			Mode:               int(mode.NativeTasks),
			Endpoint:           "POST /api/native-trials",
			BillingOperationID: "native:trial_abc",
			RequestAt:          at(2 * time.Minute),
		}},
		{label: "admin-demo-image-task", log: Log{
			RequestID: "demo-img-1", GroupID: "internal", TokenID: 9, TokenName: "demo", Code: 202,
			AsyncUsageStatus: AsyncUsageStatusCompleted, RequestSource: RequestSourceAdminDemo,
			Model: "vendor/flux", Mode: int(mode.ImagesGenerations), Endpoint: "/v1/images/tasks",
			RequestAt: at(2 * time.Minute),
		}},
		{label: "admin-demo-image-poll-404", read: true, log: Log{
			RequestID:     "image-poll-1760400000001",
			GroupID:       "internal",
			TokenID:       9,
			TokenName:     "demo",
			Code:          404,
			FailureStage:  FailureStageRouting,
			RequestSource: RequestSourceAdminDemo,
			Endpoint:      "GET /v1/images/tasks/demo-img-1",
			RequestAt:     at(time.Minute),
		}},
		// No key was recognised, so no customer owns these rows.
		{label: "unknown-key-401", read: true, log: Log{
			RequestID: "unknown-key-1", Code: 401, FailureStage: FailureStageAuth,
			Endpoint: "POST /v1/chat/completions", RequestAt: at(time.Minute),
		}},
		{label: "models-list-401", read: true, log: Log{
			RequestID: "unknown-key-2", Code: 401, FailureStage: FailureStageAuth,
			Endpoint: "GET /v1/models", RequestAt: at(time.Minute),
		}},
		{label: "other-tenant-call", log: Log{
			RequestID: "other-1", GroupID: "other", TokenID: 5, TokenName: "prod", Code: 200,
			Model: "gpt-4o", Mode: int(mode.ChatCompletions), Endpoint: "/v1/chat/completions",
			RequestAt: at(30 * time.Second),
		}},
	}
}

// seedAdminLogKindRows writes the rows and returns the label of every log ID.
func seedAdminLogKindRows(
	t *testing.T,
	db *gorm.DB,
	now time.Time,
) (map[int]string, []adminLogKindRow) {
	t.Helper()

	rows := adminLogKindRows(now)
	labels := make(map[int]string, len(rows)+1)

	for index := range rows {
		entry := rows[index].log
		entry.CreatedAt = entry.RequestAt.Add(time.Second)
		require.NoError(t, db.Create(&entry).Error, rows[index].label)
		require.Equal(t, !rows[index].read, entry.isAdminLogCall(), rows[index].label)
		rows[index].log = entry
		labels[entry.ID] = rows[index].label
	}

	// A row written before mode and endpoint were recorded is a call.
	legacy := map[string]any{
		"group_id":   "cust",
		"token_id":   7,
		"request_id": "legacy-1",
		"model":      "legacy-model",
		"mode":       nil,
		"endpoint":   nil,
		"code":       200,
		"request_at": now.Add(-20 * time.Minute),
		"created_at": now.Add(-20 * time.Minute),
	}
	require.NoError(t, db.Table("logs").Create(legacy).Error)

	var legacyRow Log
	require.NoError(t, db.Where("request_id = ?", "legacy-1").First(&legacyRow).Error)
	labels[legacyRow.ID] = "legacy-null-mode-endpoint"
	rows = append(rows, adminLogKindRow{label: "legacy-null-mode-endpoint", log: legacyRow})

	return labels, rows
}

func labelsOf(t *testing.T, labels map[int]string, logs []*Log) []string {
	t.Helper()

	got := make([]string, 0, len(logs))
	for _, entry := range logs {
		label, ok := labels[entry.ID]
		require.True(t, ok, "unexpected log %d", entry.ID)

		got = append(got, label)
	}

	slices.Sort(got)

	return got
}

func expectedAdminLogLabels(rows []adminLogKindRow, keep func(adminLogKindRow) bool) []string {
	want := []string{}

	for _, row := range rows {
		if keep(row) {
			want = append(want, row.label)
		}
	}

	slices.Sort(want)

	return want
}

func TestAdminLogKindSplitsCallsFromReads(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		db, err := OpenSQLite(filepath.Join(t.TempDir(), "admin-log-kind.db"))
		require.NoError(t, err)

		previous := LogDB
		LogDB = db
		t.Cleanup(func() {
			LogDB = previous

			if sqlDB, err := db.DB(); err == nil {
				_ = sqlDB.Close()
			}
		})

		assertAdminLogKindSplit(t, db)
	})

	t.Run("postgres", func(t *testing.T) {
		withTestPostgresStoreDB(t, func() {
			assertAdminLogKindSplit(t, LogDB)
		})
	})
}

func assertAdminLogKindSplit(t *testing.T, db *gorm.DB) {
	t.Helper()

	require.NoError(t, db.AutoMigrate(&Log{}, &RequestDetail{}, &Summary{}))

	now := time.Now()
	labels, rows := seedAdminLogKindRows(t, db, now)
	start, end := now.Add(-time.Hour), now.Add(time.Hour)

	isCall := func(row adminLogKindRow) bool { return !row.read }
	isRead := func(row adminLogKindRow) bool { return row.read }
	all := func(adminLogKindRow) bool { return true }

	getLogs := func(filter OperationalLogFilter, requestID string, perPage int) *GetLogsResult {
		page := 1

		t.Helper()

		filter.Now = now
		result, err := GetLogs(
			start,
			end,
			"",
			requestID,
			"",
			0,
			filter,
			"",
			CodeTypeAll,
			0,
			false,
			"",
			"",
			page,
			perPage,
		)
		require.NoError(t, err)

		return result
	}

	searchLogs := func(filter OperationalLogFilter, group, requestID string) *GetLogsResult {
		t.Helper()

		filter.Now = now
		result, err := SearchLogs(
			"",
			requestID,
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

	for kind, keep := range map[AdminLogKind]func(adminLogKindRow) bool{
		"":                all,
		AdminLogKindAll:   all,
		AdminLogKindCalls: isCall,
		AdminLogKindReads: isRead,
	} {
		want := expectedAdminLogLabels(rows, keep)

		result := getLogs(OperationalLogFilter{Kind: kind}, "", 100)
		require.Equal(t, want, labelsOf(t, labels, result.Logs), "GetLogs kind=%q", kind)
		require.EqualValues(t, len(want), result.Total, "GetLogs kind=%q", kind)

		result = searchLogs(OperationalLogFilter{Kind: kind}, "", "")
		require.Equal(t, want, labelsOf(t, labels, result.Logs), "SearchLogs kind=%q", kind)
		require.EqualValues(t, len(want), result.Total, "SearchLogs kind=%q", kind)
	}

	// The total counts the filtered rows, not the page.
	page := getLogs(OperationalLogFilter{Kind: AdminLogKindCalls}, "", 2)
	require.Len(t, page.Logs, 2)
	require.EqualValues(t, len(expectedAdminLogLabels(rows, isCall)), page.Total)

	// One task ID: the calls tab holds the attempt and the task row, the
	// reads tab the poll sent before the submit.
	require.Equal(
		t,
		[]string{"native-submit", "native-submit-rejected-429"},
		labelsOf(
			t,
			labels,
			getLogs(OperationalLogFilter{Kind: AdminLogKindCalls}, "job-7f3a91", 100).Logs,
		),
	)
	require.Equal(
		t,
		[]string{"native-poll-404-before-submit"},
		labelsOf(
			t,
			labels,
			searchLogs(OperationalLogFilter{Kind: AdminLogKindReads}, "", "job-7f3a91").Logs,
		),
	)

	// With a group, the rows without one (401) drop out of reads.
	require.Equal(
		t,
		[]string{
			"image-poll-404",
			"native-artifact-download-502",
			"native-poll-404-before-submit",
			"video-poll-relay-502",
		},
		labelsOf(
			t,
			labels,
			searchLogs(OperationalLogFilter{Kind: AdminLogKindReads}, "cust", "").Logs,
		),
	)

	// Kind and status combine: rejected calls are the refused attempts, not
	// the failed polls, which are rejected reads.
	require.Equal(
		t,
		[]string{"disabled-key-403", "native-submit-rejected-429"},
		labelsOf(
			t,
			labels,
			getLogs(
				OperationalLogFilter{Kind: AdminLogKindCalls, Status: CallStatusRejected},
				"",

				100,
			).Logs,
		),
	)

	for _, kind := range []AdminLogKind{AdminLogKindCalls, AdminLogKindReads} {
		for _, status := range CallStatuses {
			want := expectedAdminLogLabels(rows, func(row adminLogKindRow) bool {
				return row.read == (kind == AdminLogKindReads) && row.log.CallStatus(now) == status
			})

			result := getLogs(OperationalLogFilter{Kind: kind, Status: status}, "", 100)
			require.Equal(
				t,
				want,
				labelsOf(t, labels, result.Logs),
				"kind=%s status=%s",
				kind,
				status,
			)
			require.EqualValues(t, len(want), result.Total, "kind=%s status=%s", kind, status)
		}
	}
}
