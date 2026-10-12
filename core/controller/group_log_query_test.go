//nolint:testpackage // Group log handlers are exercised with their internal helpers.
package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type groupLogRow struct {
	ID        int    `json:"id"`
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
	TokenID   int    `json:"token_id"`
}

type groupLogPage struct {
	Logs  []groupLogRow `json:"logs"`
	Total int64         `json:"total"`
}

func newLogTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "logs.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.RequestDetail{}, &model.GroupSummary{}))

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

func serveGroupLogs(t *testing.T, group string, query url.Values) (int, groupLogPage, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "group", Value: group}}
	c.Request = httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/api/log/"+group+"?"+query.Encode(),
		nil,
	)

	GetGroupLogs(c)

	var payload struct {
		Success bool         `json:"success"`
		Data    groupLogPage `json:"data"`
	}
	if w.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload), w.Body.String())
		require.True(t, payload.Success, w.Body.String())
	}

	return w.Code, payload.Data, w.Body.String()
}

func requestIDs(page groupLogPage) []string {
	ids := make([]string, 0, len(page.Logs))
	for _, row := range page.Logs {
		ids = append(ids, row.RequestID)
	}

	slices.Sort(ids)

	return ids
}

func ms(t time.Time) string {
	return strconv.FormatInt(t.UnixMilli(), 10)
}

// seedGroupLogs writes one row per case the customer list must tell apart.
func seedGroupLogs(t *testing.T, db *gorm.DB, now time.Time) map[string]*model.Log {
	t.Helper()

	at := func(age time.Duration) time.Time { return now.Add(-age) }
	rows := map[string]*model.Log{
		"chat-ok": {
			GroupID: "g", TokenID: 3, TokenName: "prod", Code: 200, Model: "gpt-4o",
			Mode: int(mode.ChatCompletions), Endpoint: "/v1/chat/completions", RequestAt: at(time.Hour),
		},
		"chat-upstream-failed": {
			GroupID: "g", TokenID: 3, TokenName: "prod", Code: 502, FailureStage: model.FailureStageUpstream,
			Model: "gpt-4o", Mode: int(mode.ChatCompletions), Endpoint: "/v1/chat/completions", RequestAt: at(2 * time.Hour),
		},
		"balance-rejected": {
			GroupID: "g", TokenID: 4, TokenName: "ci", Code: 402, FailureStage: model.FailureStageBalance,
			Model: "gpt-4o", Mode: int(mode.ChatCompletions), Endpoint: "/v1/chat/completions", RequestAt: at(3 * time.Hour),
		},
		"failed-poll": {
			GroupID: "g", TokenID: 3, TokenName: "prod", Code: 404, FailureStage: model.FailureStageNone,
			Endpoint: "GET /v1/model-tasks/missing", RequestAt: at(4 * time.Hour),
		},
		"native-processing": {
			GroupID: "g", TokenID: 3, TokenName: "prod", Code: 202, AsyncUsageStatus: model.AsyncUsageStatusPending,
			Model: "vendor/flux", Capability: "text-to-image", Mode: int(mode.NativeTasks),
			Endpoint: "POST /v1/model-tasks", RequestAt: at(30 * time.Minute),
		},
		"measured-image-billing-pending": {
			GroupID: "g", TokenID: 3, TokenName: "prod", Code: 200, AsyncUsageStatus: model.AsyncUsageStatusPending,
			Model: "vendor/flux", Mode: int(mode.ImagesGenerations), Endpoint: "/v1/images/generations", RequestAt: at(5 * time.Hour),
		},
		"admin-demo": {
			GroupID: "g", TokenID: 3, Code: 200, RequestSource: model.RequestSourceAdminDemo,
			Model: "gpt-4o", Mode: int(mode.ChatCompletions), Endpoint: "/v1/chat/completions", RequestAt: at(6 * time.Hour),
		},
		"twenty-days-old": {
			GroupID: "g", TokenID: 3, TokenName: "prod", Code: 200, Model: "gpt-4o",
			Mode: int(mode.ChatCompletions), Endpoint: "/v1/chat/completions", RequestAt: at(20 * 24 * time.Hour),
		},
		"other-tenant": {
			GroupID: "other", TokenID: 9, Code: 502, FailureStage: model.FailureStageUpstream,
			Model: "gpt-4o", Mode: int(mode.ChatCompletions), Endpoint: "/v1/chat/completions", RequestAt: at(time.Hour),
		},
	}

	for requestID, row := range rows {
		row.RequestID = model.EmptyNullString(requestID)
		row.CreatedAt = row.RequestAt.Add(time.Second)
		require.NoError(t, db.Create(row).Error)
	}

	return rows
}

func TestGroupLogsCarryCallStatus(t *testing.T) {
	db := newLogTestDB(t)
	seedGroupLogs(t, db, time.Now())

	code, page, body := serveGroupLogs(t, "g", url.Values{"per_page": {"100"}})
	require.Equal(t, http.StatusOK, code, body)

	statuses := map[string]string{}
	for _, row := range page.Logs {
		statuses[row.RequestID] = row.Status
	}

	require.Equal(t, map[string]string{
		"chat-ok":                        "succeeded",
		"chat-upstream-failed":           "failed",
		"balance-rejected":               "rejected",
		"failed-poll":                    "failed",
		"native-processing":              "processing",
		"measured-image-billing-pending": "succeeded",
		"admin-demo":                     "succeeded",
	}, statuses)
}

func TestGroupLogsStatusFilterApplies(t *testing.T) {
	db := newLogTestDB(t)
	seedGroupLogs(t, db, time.Now())

	for status, want := range map[string][]string{
		"rejected":   {"balance-rejected"},
		"processing": {"native-processing"},
		"failed":     {"chat-upstream-failed", "failed-poll"},
		// The pre-P1 alias still filters.
		"success": {"admin-demo", "chat-ok", "measured-image-billing-pending"},
	} {
		code, page, body := serveGroupLogs(t, "g", url.Values{"status": {status}, "per_page": {"100"}})
		require.Equal(t, http.StatusOK, code, body)
		require.Equal(t, want, requestIDs(page), status)
		require.EqualValues(t, len(want), page.Total, status)
	}
}

func TestGroupLogsKindSeparatesCallsFromReads(t *testing.T) {
	db := newLogTestDB(t)
	seedGroupLogs(t, db, time.Now())

	code, page, body := serveGroupLogs(t, "g", url.Values{"kind": {"calls"}, "per_page": {"100"}})
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []string{
		"balance-rejected",
		"chat-ok",
		"chat-upstream-failed",
		"measured-image-billing-pending",
		"native-processing",
	}, requestIDs(page))

	code, page, body = serveGroupLogs(t, "g", url.Values{"kind": {"reads"}, "per_page": {"100"}})
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []string{"admin-demo", "failed-poll"}, requestIDs(page))

	code, page, body = serveGroupLogs(t, "g", url.Values{
		"kind": {"calls"}, "status": {"failed"}, "per_page": {"100"},
	})
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []string{"chat-upstream-failed"}, requestIDs(page))
}

func TestGroupLogsWindowReachesBeyondSevenDays(t *testing.T) {
	db := newLogTestDB(t)
	now := time.Now()
	seedGroupLogs(t, db, now)

	code, page, body := serveGroupLogs(t, "g", url.Values{
		"start": {ms(now.Add(-30 * 24 * time.Hour))}, "kind": {"calls"}, "per_page": {"100"},
	})
	require.Equal(t, http.StatusOK, code, body)
	require.Contains(t, requestIDs(page), "twenty-days-old")
	require.EqualValues(t, 6, page.Total)

	// Without start the legacy 7-day created_at window is unchanged.
	code, page, body = serveGroupLogs(t, "g", url.Values{"kind": {"calls"}, "per_page": {"100"}})
	require.Equal(t, http.StatusOK, code, body)
	require.NotContains(t, requestIDs(page), "twenty-days-old")
}

func TestGroupLogsWindowUsesRequestTime(t *testing.T) {
	db := newLogTestDB(t)
	now := time.Now()

	for requestID, times := range map[string][2]time.Time{
		// An image task row keeps handler time as request_at; created later.
		"requested-before-window": {now.Add(-10 * 24 * time.Hour), now.Add(-time.Minute)},
		"requested-in-window":     {now.Add(-2 * 24 * time.Hour), now.Add(-time.Minute)},
		"requested-after-end":     {now.Add(-12 * time.Hour), now.Add(-12 * time.Hour)},
	} {
		require.NoError(t, db.Create(&model.Log{
			RequestID: model.EmptyNullString(requestID), GroupID: "g", TokenID: 3, Code: 200,
			Mode: int(mode.ImagesGenerations), RequestAt: times[0], CreatedAt: times[1],
		}).Error)
	}

	code, page, body := serveGroupLogs(t, "g", url.Values{
		"start": {ms(now.Add(-5 * 24 * time.Hour))},
		"end":   {ms(now.Add(-24 * time.Hour))},
	})
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []string{"requested-in-window"}, requestIDs(page))
}

func TestGroupLogsModelIDMatchesCapabilityAndPlainModels(t *testing.T) {
	db := newLogTestDB(t)
	seedGroupLogs(t, db, time.Now())

	for modelID, want := range map[string][]string{
		"vendor/flux/text-to-image":  {"native-processing"},
		"vendor/flux":                {"measured-image-billing-pending"},
		"gpt-4o":                     {"balance-rejected", "chat-ok", "chat-upstream-failed"},
		"vendor/flux/image-to-image": {},
	} {
		code, page, body := serveGroupLogs(t, "g", url.Values{
			"model_id": {modelID}, "kind": {"calls"}, "per_page": {"100"},
		})
		require.Equal(t, http.StatusOK, code, body)
		require.Equal(t, want, requestIDs(page), modelID)
	}
}

func TestGroupLogsTokenIDFilters(t *testing.T) {
	db := newLogTestDB(t)
	seedGroupLogs(t, db, time.Now())

	code, page, body := serveGroupLogs(t, "g", url.Values{"token_id": {"4"}, "per_page": {"100"}})
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []string{"balance-rejected"}, requestIDs(page))
}

func TestGroupLogsLogIDFindsOldRowsWithinTheGroup(t *testing.T) {
	db := newLogTestDB(t)
	rows := seedGroupLogs(t, db, time.Now())

	code, page, body := serveGroupLogs(t, "g", url.Values{
		"log_id": {strconv.Itoa(rows["twenty-days-old"].ID)},
	})
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []string{"twenty-days-old"}, requestIDs(page))
	require.EqualValues(t, 1, page.Total)

	// Another group's row id is not visible through this group.
	code, page, body = serveGroupLogs(t, "g", url.Values{
		"log_id": {strconv.Itoa(rows["other-tenant"].ID)},
	})
	require.Equal(t, http.StatusOK, code, body)
	require.Empty(t, page.Logs)
	require.Zero(t, page.Total)
}

func TestGroupLogsWindowPagesAreStable(t *testing.T) {
	db := newLogTestDB(t)
	now := time.Now()
	sameMoment := now.Add(-2 * time.Hour)

	// Created order is the reverse of request order, and three rows share
	// one request time: only request_at with an id tiebreak pages stably.
	seed := []struct {
		requestID string
		requestAt time.Time
		createdAt time.Time
	}{
		{"newest-request", now.Add(-time.Hour), now.Add(-50 * time.Minute)},
		{"tie-a", sameMoment, now.Add(-40 * time.Minute)},
		{"tie-b", sameMoment, now.Add(-30 * time.Minute)},
		{"tie-c", sameMoment, now.Add(-20 * time.Minute)},
		{"oldest-request", now.Add(-3 * time.Hour), now.Add(-10 * time.Minute)},
	}
	ids := map[string]int{}

	for _, row := range seed {
		entry := &model.Log{
			RequestID: model.EmptyNullString(row.requestID), GroupID: "g", TokenID: 3, Code: 200,
			Mode: int(mode.ChatCompletions), RequestAt: row.requestAt, CreatedAt: row.createdAt,
		}
		require.NoError(t, db.Create(entry).Error)
		ids[row.requestID] = entry.ID
	}

	var ordered []string

	for pageNumber := 1; pageNumber <= 3; pageNumber++ {
		code, page, body := serveGroupLogs(t, "g", url.Values{
			"start":    {ms(now.Add(-24 * time.Hour))},
			"page":     {strconv.Itoa(pageNumber)},
			"per_page": {"2"},
		})
		require.Equal(t, http.StatusOK, code, body)
		require.EqualValues(t, 5, page.Total)

		for _, row := range page.Logs {
			ordered = append(ordered, row.RequestID)
		}
	}

	require.Equal(t, []string{"newest-request", "tie-c", "tie-b", "tie-a", "oldest-request"}, ordered)
	require.Greater(t, ids["tie-c"], ids["tie-b"])
}
