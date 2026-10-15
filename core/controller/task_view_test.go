//nolint:testpackage // The task view handlers are exercised with the group log test helpers.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// seedTaskViewLogs writes one native task with a rejected retry, a submit
// that failed after the hold and its successful resend, an image-trace
// rejection, a poll for an unknown ID, a sync call and an unidentified 401.
func seedTaskViewLogs(t *testing.T, db *gorm.DB, now time.Time) map[string]*model.Log {
	t.Helper()

	native := func(id string, token int) model.Log {
		return model.Log{
			RequestID: model.EmptyNullString(id), GroupID: "g", TokenID: token, Model: "vendor/flux",
			Mode: int(mode.NativeTasks), Endpoint: "POST /v1/model-tasks",
		}
	}

	rows := map[string]model.Log{}

	rejected := native("job-a1234", 3)
	rejected.Code, rejected.FailureStage, rejected.ErrorCode = 429, model.FailureStageRateLimit, "too_many_active_tasks"
	rejected.RequestAt = now.Add(-50 * time.Minute)
	rows["a-rejected-429"] = rejected

	ownA := native("job-a1234", 3)
	ownA.Code, ownA.AsyncUsageStatus, ownA.BillingOperationID = 202, model.AsyncUsageStatusCompleted, "native:job-a1234"
	ownA.RequestAt = now.Add(-49 * time.Minute)
	rows["a-own"] = ownA

	failed := native("job-b5678", 4)
	failed.Code, failed.FailureStage, failed.ErrorCode = 503, model.FailureStageRouting, "native_execution_unavailable"
	failed.BillingOperationID, failed.RequestAt = "native:job-b5678", now.Add(-40*time.Minute)
	rows["b-failed-after-hold"] = failed

	ownB := native("job-b5678", 4)
	ownB.Code, ownB.AsyncUsageStatus, ownB.BillingOperationID = 202, model.AsyncUsageStatusCompleted, "native:job-b5678"
	ownB.RequestAt = now.Add(-39 * time.Minute)
	rows["b-own"] = ownB

	rows["image-trace-429"] = model.Log{
		RequestID: "image-trace-1760000000000001", GroupID: "g", TokenID: 3, Endpoint: "POST /v1/images/tasks",
		Code: 429, FailureStage: model.FailureStageRateLimit, RequestAt: now.Add(-30 * time.Minute),
	}
	rows["poll-unknown-404"] = model.Log{
		RequestID: "poll-1", GroupID: "g", TokenID: 3, Endpoint: "GET /v1/model-tasks/nope", Code: 404,
		FailureStage: model.FailureStageValidation, ErrorCode: "task_not_found", RequestAt: now.Add(-20 * time.Minute),
	}
	rows["chat-ok"] = model.Log{
		RequestID: "chat-ok", GroupID: "g", TokenID: 3, Code: 200, Model: "gpt-4o",
		Mode: int(mode.ChatCompletions), Endpoint: "/v1/chat/completions", RequestAt: now.Add(-10 * time.Minute),
	}
	rows["unknown-key-401"] = model.Log{
		RequestID: "unknown-key", Code: 401, FailureStage: model.FailureStageAuth,
		Endpoint: "POST /v1/chat/completions", RequestAt: now.Add(-5 * time.Minute),
	}

	stored := make(map[string]*model.Log, len(rows))
	for label, row := range rows {
		entry := row
		entry.CreatedAt = entry.RequestAt.Add(time.Second)
		require.NoError(t, db.Create(&entry).Error, label)
		stored[label] = &entry
	}

	return stored
}

// labelsOfPage returns the sorted labels of a page's rows.
func labelsOfPage(stored map[string]*model.Log, page groupLogPage) []string {
	byID := map[int]string{}
	for label, entry := range stored {
		byID[entry.ID] = label
	}

	out := []string{}

	for _, row := range page.Logs {
		out = append(out, byID[row.ID])
	}

	slices.Sort(out)

	return out
}

// withTaskViewReadiness replaces the task view readiness checks for one test.
func withTaskViewReadiness(t *testing.T, check func(context.Context, *gorm.DB) (bool, error)) {
	t.Helper()

	oldView, oldRequests := taskViewStorage, taskRequestsStorage
	taskViewStorage = &featureReadiness{name: "test view", check: check, now: time.Now}
	taskRequestsStorage = &featureReadiness{name: "test requests", check: check, now: time.Now}

	t.Cleanup(func() { taskViewStorage, taskRequestsStorage = oldView, oldRequests })
}

func TestGroupLogsTaskViewKinds(t *testing.T) {
	db := newLogTestDB(t)
	now := time.Now()
	stored := seedTaskViewLogs(t, db, now)
	window := url.Values{"start": {ms(now.Add(-2 * time.Hour))}, "per_page": {"100"}}

	query := func(extra url.Values) url.Values {
		out := url.Values{}
		for key, values := range window {
			out[key] = values
		}

		for key, values := range extra {
			out[key] = values
		}

		return out
	}

	code, page, body := serveGroupLogs(t, "g", query(url.Values{"kind": {"tasks"}}))
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []string{"a-own", "b-own", "chat-ok"}, labelsOfPage(stored, page))
	require.EqualValues(t, 3, page.Total)

	code, page, body = serveGroupLogs(t, "g", query(url.Values{"kind": {"non_task"}}))
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t,
		[]string{"a-rejected-429", "b-failed-after-hold", "image-trace-429", "poll-unknown-404"},
		labelsOfPage(stored, page))
	require.EqualValues(t, 4, page.Total)

	code, page, body = serveGroupLogs(t, "g", query(url.Values{"kind": {"tasks"}, "status": {"succeeded"}}))
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []string{"a-own", "b-own", "chat-ok"}, labelsOfPage(stored, page))

	// A task row is never rejected and non_task takes no status; unknown
	// kinds fail closed.
	for _, extra := range []url.Values{
		{"kind": {"tasks"}, "status": {"rejected"}},
		{"kind": {"non_task"}, "status": {"failed"}},
		{"kind": {"non_task"}, "status": {"success"}},
		{"kind": {"task"}},
		{"kind": {"non-task"}},
	} {
		code, _, body := serveGroupLogs(t, "g", query(extra))
		require.Equal(t, http.StatusBadRequest, code, "%v: %s", extra, body)
	}

	// Without the new kinds the list is unchanged: calls keep every attempt.
	code, page, body = serveGroupLogs(t, "g", query(url.Values{"kind": {"calls"}}))
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t,
		[]string{"a-own", "a-rejected-429", "b-failed-after-hold", "b-own", "chat-ok", "image-trace-429"},
		labelsOfPage(stored, page))
}

func TestAdminLogsTaskViewKinds(t *testing.T) {
	db := newLogTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Summary{}))

	now := time.Now()
	stored := seedTaskViewLogs(t, db, now)

	for _, tc := range []struct {
		name    string
		handler gin.HandlerFunc
		path    string
		query   url.Values
		want    []string
	}{
		{"list tasks", GetLogs, "/api/logs/", url.Values{"kind": {"tasks"}},
			[]string{"a-own", "b-own", "chat-ok"}},
		{"list non_task keeps unidentified callers", GetLogs, "/api/logs/", url.Values{"kind": {"non_task"}},
			[]string{"a-rejected-429", "b-failed-after-hold", "image-trace-429", "poll-unknown-404", "unknown-key-401"}},
		{"search tasks of one group", SearchLogs, "/api/logs/search", url.Values{"kind": {"tasks"}, "group": {"g"}},
			[]string{"a-own", "b-own", "chat-ok"}},
		{"search non_task of one group", SearchLogs, "/api/logs/search", url.Values{"kind": {"non_task"}, "group": {"g"}},
			[]string{"a-rejected-429", "b-failed-after-hold", "image-trace-429", "poll-unknown-404"}},
		{"search tasks by request id", SearchLogs, "/api/logs/search", url.Values{"kind": {"tasks"}, "request_id": {"job-a1234"}},
			[]string{"a-own"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.query.Set("per_page", "100")

			code, page, body := serveAdminLogs(t, tc.handler, tc.path, tc.query)
			require.Equal(t, http.StatusOK, code, body)
			require.Equal(t, tc.want, labelsOfPage(stored, page))
			require.EqualValues(t, len(tc.want), page.Total)
		})
	}

	for _, handler := range []gin.HandlerFunc{GetLogs, SearchLogs} {
		for _, query := range []url.Values{
			{"kind": {"tasks"}, "status": {"rejected"}},
			{"kind": {"non_task"}, "status": {"failed"}},
		} {
			code, _, body := serveAdminLogs(t, handler, "/api/logs/", query)
			require.Equal(t, http.StatusBadRequest, code, body)
		}
	}
}

type usageCallsPayload struct {
	Data struct {
		Totals model.CallCounts `json:"totals"`
		Unit   *string          `json:"unit"`
		Keys   []struct {
			TokenID int `json:"token_id"`
			model.CallCounts
		} `json:"keys"`
	} `json:"data"`
}

func TestUsageUnitTasks(t *testing.T) {
	db := newLogTestDB(t)
	now := time.Now()
	seedTaskViewLogs(t, db, now)

	window := url.Values{
		"start": {strconv.FormatInt(now.Add(-2*time.Hour).UnixMilli(), 10)},
		"end":   {strconv.FormatInt(now.UnixMilli(), 10)},
	}

	usage := func(handler gin.HandlerFunc, path string, extra url.Values) (int, usageCallsPayload, string) {
		t.Helper()

		query := url.Values{}
		for key, values := range window {
			query[key] = values
		}

		for key, values := range extra {
			query[key] = values
		}

		code, body := serveUsage(t, handler, path, query)

		var payload usageCallsPayload
		if code == http.StatusOK {
			require.NoError(t, json.Unmarshal(body, &payload), string(body))
		}

		return code, payload, string(body)
	}

	// Without unit: as before, and no unit field.
	code, byRequest, body := usage(GetGroupUsageCalls, "/api/usage/g/calls", nil)
	require.Equal(t, http.StatusOK, code, body)
	require.Nil(t, byRequest.Data.Unit)
	// The failed poll is a read and the 401 has no group: not calls.
	require.EqualValues(t, 6, byRequest.Data.Totals.Requests)
	require.EqualValues(t, 1, byRequest.Data.Totals.Failed)
	require.EqualValues(t, 3, byRequest.Data.Totals.Succeeded)
	require.EqualValues(t, 2, byRequest.Data.Totals.Rejected)

	code, explicit, body := usage(GetGroupUsageCalls, "/api/usage/g/calls", url.Values{"unit": {"requests"}})
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, "requests", *explicit.Data.Unit)
	require.Equal(t, byRequest.Data.Totals, explicit.Data.Totals)

	// unit=tasks: the failed-then-resent submit counts once; rejected calls
	// still count.
	code, byTask, body := usage(GetGroupUsageCalls, "/api/usage/g/calls", url.Values{"unit": {"tasks"}})
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, "tasks", *byTask.Data.Unit)
	require.EqualValues(t, 5, byTask.Data.Totals.Requests)
	require.EqualValues(t, 0, byTask.Data.Totals.Failed)
	require.EqualValues(t, 3, byTask.Data.Totals.Succeeded)
	require.EqualValues(t, 2, byTask.Data.Totals.Rejected)

	keys := url.Values{"token_ids": {"4,3"}}
	code, keyRequests, body := usage(GetGroupUsageKeys, "/api/usage/g/keys", keys)
	require.Equal(t, http.StatusOK, code, body)
	require.Nil(t, keyRequests.Data.Unit)
	require.EqualValues(t, 2, keyRequests.Data.Keys[0].Requests)

	keys.Set("unit", "tasks")
	code, keyTasks, body := usage(GetGroupUsageKeys, "/api/usage/g/keys", keys)
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, "tasks", *keyTasks.Data.Unit)
	require.Equal(t, 4, keyTasks.Data.Keys[0].TokenID)
	require.EqualValues(t, 1, keyTasks.Data.Keys[0].Requests)
	require.EqualValues(t, 1, keyTasks.Data.Keys[0].Succeeded)
	require.Equal(t, keyRequests.Data.Keys[1].Requests, keyTasks.Data.Keys[1].Requests)

	for _, path := range []string{"/api/usage/g/calls", "/api/usage/g/keys"} {
		handler := GetGroupUsageCalls
		if path == "/api/usage/g/keys" {
			handler = GetGroupUsageKeys
		}

		for _, unit := range []string{"task", "TASKS", "calls"} {
			code, _, body := usage(handler, path, url.Values{"unit": {unit}, "token_ids": {"3"}})
			require.Equal(t, http.StatusBadRequest, code, "%s unit=%s: %s", path, unit, body)
		}
	}
}

// Before the task view's storage is ready the new kinds and unit=tasks answer
// 503 task_view_unavailable, /api/status stays 200 without the feature, and
// requests without the new parameters are served as before.
func TestTaskViewFailsClosedWhenNotReady(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := statusLogDB(t)
	require.NoError(t, db.AutoMigrate(&model.RequestDetail{}, &model.GroupSummary{}))

	probes := 0
	withTaskViewReadiness(t, func(context.Context, *gorm.DB) (bool, error) {
		probes++
		return false, errors.New("indexes missing")
	})

	recorder := getStatus(t)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), model.AdminDemoFeature)
	require.NotContains(t, recorder.Body.String(), model.LogTaskViewFeature)

	// While unready the probe runs at most every 10 s.
	before := probes
	require.Equal(t, http.StatusOK, getStatus(t).Code)
	require.Equal(t, before, probes)

	for _, kind := range []string{"tasks", "non_task"} {
		code, _, body := serveGroupLogs(t, "g", url.Values{"kind": {kind}})
		require.Equal(t, http.StatusServiceUnavailable, code, body)
		require.Contains(t, body, errTaskViewUnavailable)

		code, _, body = serveAdminLogs(t, GetLogs, "/api/logs/", url.Values{"kind": {kind}})
		require.Equal(t, http.StatusServiceUnavailable, code, body)

		code, _, body = serveAdminLogs(t, SearchLogs, "/api/logs/search", url.Values{"kind": {kind}})
		require.Equal(t, http.StatusServiceUnavailable, code, body)
	}

	now := time.Now()
	window := url.Values{
		"start":     {strconv.FormatInt(now.Add(-time.Hour).UnixMilli(), 10)},
		"end":       {strconv.FormatInt(now.UnixMilli(), 10)},
		"token_ids": {"3"},
	}

	code, _ := serveUsage(t, GetGroupUsageCalls, "/api/usage/g/calls", window)
	require.Equal(t, http.StatusOK, code)

	window.Set("unit", "tasks")
	code, body := serveUsage(t, GetGroupUsageCalls, "/api/usage/g/calls", window)
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	code, body = serveUsage(t, GetGroupUsageKeys, "/api/usage/g/keys", window)
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))

	code, _, body2 := serveGroupLogs(t, "g", url.Values{"kind": {"calls"}})
	require.Equal(t, http.StatusOK, code, body2)

	code, related := serveTaskRequests(t, "g", url.Values{"log_id": {"1"}})
	require.Equal(t, http.StatusServiceUnavailable, code, related)
}

func serveTaskRequests(t *testing.T, group string, query url.Values) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "group", Value: group}}
	c.Request = httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/api/log/"+group+"/task-requests?"+query.Encode(),
		nil,
	)

	GetGroupLogTaskRequests(c)

	return w.Code, w.Body.String()
}

func TestGroupLogTaskRequests(t *testing.T) {
	db := newLogTestDB(t)
	now := time.Now()
	stored := seedTaskViewLogs(t, db, now)

	// Without the task tables the endpoint is unavailable.
	code, body := serveTaskRequests(t, "g", url.Values{"log_id": {strconv.Itoa(stored["a-own"].ID)}})
	require.Equal(t, http.StatusServiceUnavailable, code, body)

	require.NoError(t, db.AutoMigrate(&model.NativeTask{}, &model.ImageTask{}))

	for _, task := range []model.NativeTask{
		{ID: "job-a1234", GroupID: "g", TokenID: 3, BillingOperationID: "native:job-a1234", Model: "vendor/flux", Status: "completed"},
		{ID: "job-b5678", GroupID: "g", TokenID: 4, BillingOperationID: "native:job-b5678", Model: "vendor/flux", Status: "completed"},
	} {
		require.NoError(t, db.Create(&task).Error)
	}

	// The readiness probe ran before the tables existed; force a recheck.
	withTaskViewReadiness(t, model.TaskRequestsStorageReady)

	var payload struct {
		Data struct {
			Task     struct{ ID int } `json:"task"`
			Attempts []struct {
				Relation string `json:"relation"`
				Hint     string `json:"hint"`
				Log      struct {
					ID     int    `json:"id"`
					Status string `json:"status"`
				} `json:"log"`
			} `json:"attempts"`
			Truncated bool `json:"truncated"`
		} `json:"data"`
	}

	code, body = serveTaskRequests(t, "g", url.Values{"log_id": {strconv.Itoa(stored["a-own"].ID)}})
	require.Equal(t, http.StatusOK, code, body)
	require.NoError(t, json.Unmarshal([]byte(body), &payload))
	require.Equal(t, stored["a-own"].ID, payload.Data.Task.ID)
	require.Len(t, payload.Data.Attempts, 1)
	require.Equal(t, "rejected", payload.Data.Attempts[0].Relation)
	require.Equal(t, "before_submit", payload.Data.Attempts[0].Hint)
	require.Equal(t, stored["a-rejected-429"].ID, payload.Data.Attempts[0].Log.ID)
	require.Equal(t, "rejected", payload.Data.Attempts[0].Log.Status)
	require.False(t, payload.Data.Truncated)

	code, body = serveTaskRequests(t, "g", url.Values{"log_id": {strconv.Itoa(stored["b-own"].ID)}})
	require.Equal(t, http.StatusOK, code, body)
	require.NoError(t, json.Unmarshal([]byte(body), &payload))
	require.Len(t, payload.Data.Attempts, 1)
	require.Equal(t, "failed_submit", payload.Data.Attempts[0].Relation)
	require.Equal(t, stored["b-failed-after-hold"].ID, payload.Data.Attempts[0].Log.ID)

	for _, tc := range []struct {
		group string
		query url.Values
		want  int
	}{
		{"g", url.Values{}, http.StatusBadRequest},
		{"g", url.Values{"log_id": {"0"}}, http.StatusBadRequest},
		{"g", url.Values{"log_id": {"abc"}}, http.StatusBadRequest},
		// Not a native or image task row.
		{"g", url.Values{"log_id": {strconv.Itoa(stored["chat-ok"].ID)}}, http.StatusBadRequest},
		{"g", url.Values{"log_id": {strconv.Itoa(stored["a-rejected-429"].ID)}}, http.StatusBadRequest},
		{"g", url.Values{"log_id": {strconv.Itoa(stored["b-failed-after-hold"].ID)}}, http.StatusBadRequest},
		// Another group never sees the row.
		{"other", url.Values{"log_id": {strconv.Itoa(stored["a-own"].ID)}}, http.StatusNotFound},
		{"g", url.Values{"log_id": {"999999"}}, http.StatusNotFound},
	} {
		code, body := serveTaskRequests(t, tc.group, tc.query)
		require.Equal(t, tc.want, code, "%s %v: %s", tc.group, tc.query, body)
	}
}
