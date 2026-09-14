package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestGetRequestTraceReturnsOnlySafeTraceProjection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := installTraceControllerDB(t)
	span := controllerTraceSpan(1)
	channelID := 17
	span.Attributes.ChannelID = &channelID
	require.NoError(t, store.Write(context.Background(), span))

	engine := gin.New()
	engine.GET("/api/trace/:group/:trace_id", GetRequestTrace)
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/trace/group-one/%s?service=aiproxy", controllerTraceID(1)), nil)
	request.Header.Set("Authorization", "Bearer any-header-is-checked-by-router")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			Items []struct {
				TraceID    string `json:"trace_id"`
				SpanID     string `json:"span_id"`
				RequestID  string `json:"request_id"`
				Attributes struct {
					ChannelID *int `json:"channel_id"`
				} `json:"attributes"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
			Truncated  bool   `json:"truncated"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.True(t, payload.Success)
	require.Len(t, payload.Data.Items, 1)
	require.Equal(t, span.TraceID, payload.Data.Items[0].TraceID)
	require.Equal(t, span.SpanID, payload.Data.Items[0].SpanID)
	require.Equal(t, span.RequestID, payload.Data.Items[0].RequestID)
	require.Equal(t, channelID, *payload.Data.Items[0].Attributes.ChannelID)
	require.Empty(t, payload.Data.NextCursor)
	require.False(t, payload.Data.Truncated)
	for _, forbidden := range []string{"request_body", "response_body", "token", "cookie", "url", "prompt"} {
		require.NotContains(t, recorder.Body.String(), forbidden)
	}
}

func TestGetRequestTraceRejectsMissingHeaderAndInvalidParameters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/api/trace/:group/:trace_id", GetRequestTrace)

	cases := []string{
		fmt.Sprintf("/api/trace/group-one/%s?service=aiproxy", controllerTraceID(1)),
		fmt.Sprintf("/api/trace/group-one/%s?service=other", controllerTraceID(1)),
		fmt.Sprintf("/api/trace/group-one/%s?service=aiproxy", strings.ToUpper(controllerTraceID(10))),
		fmt.Sprintf("/api/trace/group-one/%s?service=aiproxy&after=%s", controllerTraceID(1), strings.ToUpper(controllerTraceID(10))),
		fmt.Sprintf("/api/trace/group-one/%s?service=aiproxy&limit=0", controllerTraceID(1)),
		fmt.Sprintf("/api/trace/group-one/%s?service=aiproxy&limit=101", controllerTraceID(1)),
		fmt.Sprintf("/api/trace/%%01/%s?service=aiproxy", controllerTraceID(1)),
	}
	for index, path := range cases {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			if index != 0 {
				request.Header.Set("Authorization", "Bearer admin")
			}
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)
			if index == 0 {
				require.Equal(t, http.StatusUnauthorized, recorder.Code)
			} else {
				require.Equal(t, http.StatusBadRequest, recorder.Code)
			}
		})
	}
}

func TestGetRequestTraceHidesStoreFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousLogDB := model.LogDB
	model.LogDB = nil
	t.Cleanup(func() { model.LogDB = previousLogDB })

	engine := gin.New()
	engine.GET("/api/trace/:group/:trace_id", GetRequestTrace)
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/trace/group-one/%s?service=aiproxy", controllerTraceID(1)), nil)
	request.Header.Set("Authorization", "Bearer admin")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.JSONEq(t, `{"success":false,"message":"trace_unavailable"}`, recorder.Body.String())
}

func installTraceControllerDB(t *testing.T) *model.TraceStore {
	t.Helper()
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "trace-controller.db"))
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	previousLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = previousLogDB
		require.NoError(t, sqlDB.Close())
	})
	store := model.NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))
	return store
}

func controllerTraceSpan(sequence int) requesttrace.Span {
	traceID := controllerTraceID(1)
	spanID := controllerTraceID(2)
	if sequence != 1 {
		traceID = controllerTraceID(3)
		spanID = controllerTraceID(4)
	}
	return requesttrace.Span{
		Version: requesttrace.Version, TraceID: traceID, SpanID: spanID,
		RequestID: "request-safe", GroupID: "group-one", Service: requesttrace.ServiceAIProxy,
		Stage: requesttrace.StageRequest, Status: requesttrace.StatusRunning,
		StartedAt: time.Date(2026, 9, 8, 1, 2, 3, 0, time.UTC), Revision: 1,
	}
}

func controllerTraceID(value int) string { return fmt.Sprintf("%032x", value) }
