package model_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newUsageStatsDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "usage.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = old

		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	return db
}

func seedUsageLog(t *testing.T, db *gorm.DB, entry model.Log) model.Log {
	t.Helper()

	if entry.GroupID == "" {
		entry.GroupID = "g"
	}

	if entry.Mode == 0 && entry.Endpoint == "" {
		entry.Mode = int(mode.ChatCompletions)
		entry.Endpoint = "/v1/chat/completions"
	}

	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = entry.RequestAt.Add(time.Second)
	}

	require.NoError(t, db.Create(&entry).Error)

	return entry
}

func ptr(value int64) *int64 { return &value }

func TestGroupCallTotalsBySource(t *testing.T) {
	db := newUsageStatsDB(t)
	now := time.Now()
	start, end := now.Add(-30*24*time.Hour), now

	latest := now.Add(-time.Hour)
	for _, entry := range []model.Log{
		// An empty source is an api call written before sources existed.
		{TokenID: 1, Code: 200, RequestSource: "", RequestAt: latest},
		{TokenID: 1, Code: 502, FailureStage: model.FailureStageUpstream, RequestSource: model.RequestSourceAPI, RequestAt: now.Add(-2 * time.Hour)},
		{TokenID: 1, Code: 402, FailureStage: model.FailureStageBalance, RequestSource: model.RequestSourceAPI, RequestAt: now.Add(-3 * time.Hour)},
		{TokenID: 2, Code: 202, AsyncUsageStatus: model.AsyncUsageStatusPending, Mode: int(mode.NativeTasks), Endpoint: "POST /v1/model-tasks", RequestSource: model.RequestSourcePlayground, RequestAt: now.Add(-4 * time.Hour)},
		// Pending for more than 24 h: unknown, still a call.
		{TokenID: 2, Code: 202, AsyncUsageStatus: model.AsyncUsageStatusPending, Mode: int(mode.NativeTasks), Endpoint: "POST /v1/model-tasks", RequestSource: model.RequestSourceAPI, RequestAt: now.Add(-48 * time.Hour)},
		// Never calls: an internal demo, a failed poll, a relay read, an
		// unrecognised key, another group, outside the window.
		{TokenID: 1, Code: 200, RequestSource: model.RequestSourceAdminDemo, RequestAt: now.Add(-time.Minute)},
		{TokenID: 1, Code: 404, Endpoint: "GET /v1/model-tasks/x", RequestAt: now.Add(-time.Minute)},
		{TokenID: 1, Code: 502, Mode: int(mode.VideosGet), Endpoint: "/v1/videos/x", RequestAt: now.Add(-time.Minute)},
		{TokenID: 0, Code: 401, FailureStage: model.FailureStageAuth, RequestAt: now.Add(-time.Minute)},
		{GroupID: "other", TokenID: 9, Code: 200, RequestAt: now.Add(-time.Minute)},
		{TokenID: 1, Code: 200, RequestAt: now.Add(-40 * 24 * time.Hour)},
	} {
		seedUsageLog(t, db, entry)
	}

	totals, err := model.GetGroupCallTotals(context.Background(), "g", start, end, now)
	require.NoError(t, err)

	lastMS := latest.UnixMilli()
	require.Equal(t, model.CallCounts{
		Requests: 5, Succeeded: 1, Failed: 1, Rejected: 1, Processing: 1, Unknown: 1,
		LastRequestAt: &lastMS,
	}, totals.Totals)

	require.Equal(t, []model.SourceCallCounts{
		{Source: "api", CallCounts: model.CallCounts{
			Requests: 4, Succeeded: 1, Failed: 1, Rejected: 1, Unknown: 1, LastRequestAt: &lastMS,
		}},
		{Source: "playground", CallCounts: model.CallCounts{
			Requests: 1, Processing: 1, LastRequestAt: ptr(now.Add(-4 * time.Hour).UnixMilli()),
		}},
	}, totals.BySource)

	empty, err := model.GetGroupCallTotals(context.Background(), "nobody", start, end, now)
	require.NoError(t, err)
	require.Equal(t, model.CallCounts{}, empty.Totals)
	require.Empty(t, empty.BySource)
}

func TestGroupKeyCallStats(t *testing.T) {
	db := newUsageStatsDB(t)
	now := time.Now()
	start, end := now.Add(-30*24*time.Hour), now

	// Key 3 was renamed: its latest call carries the new name.
	seedUsageLog(t, db, model.Log{TokenID: 3, TokenName: "old-name", Code: 200, RequestAt: now.Add(-5 * time.Hour)})
	seedUsageLog(t, db, model.Log{TokenID: 3, TokenName: "prod", Code: 502, FailureStage: model.FailureStageUpstream, RequestAt: now.Add(-2 * time.Hour)})
	seedUsageLog(t, db, model.Log{TokenID: 3, TokenName: "prod", Code: 200, RequestAt: now.Add(-time.Hour)})
	// A failed poll after the last call is not a call and does not move last used.
	seedUsageLog(t, db, model.Log{TokenID: 3, TokenName: "prod", Code: 404, Endpoint: "GET /v1/model-tasks/x", RequestAt: now.Add(-time.Minute)})
	// Key 5 is idle in the window; its last call is older.
	idleAt := now.Add(-90 * 24 * time.Hour)
	seedUsageLog(t, db, model.Log{TokenID: 5, TokenName: "idle", Code: 200, RequestAt: idleAt})
	// Key 9 belongs to another group.
	seedUsageLog(t, db, model.Log{GroupID: "other", TokenID: 9, TokenName: "theirs", Code: 200, RequestAt: now.Add(-time.Hour)})

	stats, err := model.GetGroupKeyCallStats(context.Background(), "g", []int{9, 3, 7, 5}, start, end, now)
	require.NoError(t, err)

	require.Equal(t, []model.KeyCallStats{
		{TokenID: 9},
		{TokenID: 3, CallCounts: model.CallCounts{
			Requests: 3, Succeeded: 2, Failed: 1, LastRequestAt: ptr(now.Add(-time.Hour).UnixMilli()),
		}, LastTokenName: "prod"},
		{TokenID: 7},
		{TokenID: 5, CallCounts: model.CallCounts{LastRequestAt: ptr(idleAt.UnixMilli())}, LastTokenName: "idle"},
	}, stats)
}
