//nolint:testpackage // The admin log handlers are exercised with the group log test helpers.
package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

func serveAdminLogs(
	t *testing.T,
	handler gin.HandlerFunc,
	path string,
	query url.Values,
) (int, groupLogPage, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		path+"?"+query.Encode(),
		nil,
	)

	handler(c)

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

func TestAdminLogsKindSeparatesCallsFromReads(t *testing.T) {
	db := newLogTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Summary{}))

	now := time.Now()
	for requestID, row := range map[string]*model.Log{
		"chat-ok": {
			GroupID: "g", TokenID: 3, Code: 200, Model: "gpt-4o",
			Mode: int(mode.ChatCompletions), Endpoint: "/v1/chat/completions",
		},
		"trial_abc": {
			GroupID: "internal", TokenID: 4, TokenName: "trial_ops", Code: 202,
			RequestSource: model.RequestSourceAdminDemo, Model: "vendor/flux",
			Mode: int(mode.NativeTasks), Endpoint: "POST /api/native-trials",
		},
		"failed-poll": {
			GroupID: "g", TokenID: 3, Code: 404, FailureStage: model.FailureStageValidation,
			Endpoint: "GET /v1/model-tasks/missing",
		},
		"unknown-key": {
			Code: 401, FailureStage: model.FailureStageAuth, Endpoint: "POST /v1/chat/completions",
		},
	} {
		row.RequestID = model.EmptyNullString(requestID)
		row.RequestAt = now.Add(-time.Hour)
		row.CreatedAt = row.RequestAt.Add(time.Second)
		require.NoError(t, db.Create(row).Error)
	}

	for _, tc := range []struct {
		name    string
		handler gin.HandlerFunc
		path    string
		query   url.Values
		want    []string
	}{
		{"list without kind keeps every row", GetLogs, "/api/logs/", url.Values{},
			[]string{"chat-ok", "failed-poll", "trial_abc", "unknown-key"}},
		{"list calls keeps trials", GetLogs, "/api/logs/", url.Values{"kind": {"calls"}},
			[]string{"chat-ok", "trial_abc"}},
		{"list reads holds polls and unknown keys", GetLogs, "/api/logs/", url.Values{"kind": {"reads"}},
			[]string{"failed-poll", "unknown-key"}},
		{"search reads of one group", SearchLogs, "/api/logs/search", url.Values{"kind": {"reads"}, "group": {"g"}},
			[]string{"failed-poll"}},
		{"search calls", SearchLogs, "/api/logs/search", url.Values{"kind": {"calls"}},
			[]string{"chat-ok", "trial_abc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.query.Set("per_page", "100")

			code, page, body := serveAdminLogs(t, tc.handler, tc.path, tc.query)
			require.Equal(t, http.StatusOK, code, body)
			require.Equal(t, tc.want, requestIDs(page))
			require.EqualValues(t, len(tc.want), page.Total)
		})
	}

	for _, path := range []string{"/api/logs/", "/api/logs/search"} {
		handler := GetLogs
		if path == "/api/logs/search" {
			handler = SearchLogs
		}

		code, _, body := serveAdminLogs(t, handler, path, url.Values{"kind": {"tasks"}})
		require.Equal(t, http.StatusBadRequest, code, body)
	}
}
