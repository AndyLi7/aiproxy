package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestRequestTraceRouteRequiresAdminHeaderAndValidatesInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine, traceID := requestTraceRouter(t, false)

	for _, testCase := range []struct {
		name   string
		path   string
		header string
		want   int
	}{
		{"missing header", fmt.Sprintf("/api/trace/group-one/%s?service=aiproxy", traceID), "", http.StatusUnauthorized},
		{"ordinary client key", fmt.Sprintf("/api/trace/group-one/%s?service=aiproxy", traceID), "Bearer client-key", http.StatusUnauthorized},
		{"query admin key is not accepted", fmt.Sprintf("/api/trace/group-one/%s?service=aiproxy&key=admin-key", traceID), "", http.StatusUnauthorized},
		{"invalid service", fmt.Sprintf("/api/trace/group-one/%s?service=other", traceID), "Bearer admin-key", http.StatusBadRequest},
		{"invalid limit", fmt.Sprintf("/api/trace/group-one/%s?service=aiproxy&limit=101", traceID), "Bearer admin-key", http.StatusBadRequest},
		{"invalid trace ID", "/api/trace/group-one/not-a-trace?service=aiproxy", "Bearer admin-key", http.StatusBadRequest},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, testCase.path, nil)
			if testCase.header != "" {
				request.Header.Set("Authorization", testCase.header)
			}
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)
			require.Equal(t, testCase.want, recorder.Code)
		})
	}
}

func TestRequestTraceRouteReturnsUnavailableWithoutDatabaseDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousLogDB := model.LogDB
	model.LogDB = nil
	t.Cleanup(func() { model.LogDB = previousLogDB })
	previousAdminKey := config.AdminKey
	config.AdminKey = "admin-key"
	t.Cleanup(func() { config.AdminKey = previousAdminKey })

	engine := gin.New()
	SetAPIRouter(engine)
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/trace/group-one/%s?service=aiproxy", requestTraceID(1)), nil)
	request.Header.Set("Authorization", "Bearer admin-key")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.JSONEq(t, `{"success":false,"message":"trace_unavailable"}`, recorder.Body.String())
}

func TestRequestTraceRouteReadsWriterRetriedLifecycleRefresh(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine, traceID := requestTraceRouter(t, true)
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/trace/group-one/%s?service=aiproxy", traceID), nil)
	request.Header.Set("Authorization", "Bearer admin-key")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			Items []struct {
				SpanID   string `json:"span_id"`
				Status   string `json:"status"`
				Revision int    `json:"revision"`
			} `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.True(t, payload.Success)
	require.Len(t, payload.Data.Items, 1)
	require.Equal(t, "success", payload.Data.Items[0].Status)
	require.Equal(t, 2, payload.Data.Items[0].Revision)
	for _, forbidden := range []string{"request_body", "response_body", "token", "cookie", "url", "prompt"} {
		require.NotContains(t, recorder.Body.String(), forbidden)
	}
}

func requestTraceRouter(t *testing.T, writeLifecycle bool) (*gin.Engine, string) {
	t.Helper()
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "request-trace-router.db"))
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

	previousAdminKey := config.AdminKey
	config.AdminKey = "admin-key"
	t.Cleanup(func() { config.AdminKey = previousAdminKey })

	traceID := requestTraceID(1)
	if writeLifecycle {
		sink := &retryTraceSink{store: store, groupID: "group-one"}
		writer := requesttrace.NewWriter(sink, requesttrace.WriterOptions{MaxAttempts: 2, WriteTimeout: time.Second})
		recorder := requesttrace.NewRecorder("router-trace-request", writer.Submit)
		handle := recorder.Begin(requesttrace.StageRequest, "")
		require.True(t, handle.Finish(requesttrace.StatusSuccess))
		require.NoError(t, writer.Close(context.Background()))
		require.Equal(t, int32(3), sink.calls.Load(), "the initial write retries once and the completion refreshes the same span")
		traceID = recorder.TraceID()
	}

	engine := gin.New()
	SetAPIRouter(engine)
	return engine, traceID
}

type retryTraceSink struct {
	store   *model.TraceStore
	groupID string
	calls   atomic.Int32
}

func (s *retryTraceSink) Write(ctx context.Context, span requesttrace.Span) error {
	span.GroupID = s.groupID
	if s.calls.Add(1) == 1 {
		return errors.New("temporary trace store failure")
	}
	return s.store.Write(ctx, span)
}

func requestTraceID(value int) string { return fmt.Sprintf("%032x", value) }
