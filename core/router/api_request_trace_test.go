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
	"github.com/labring/aiproxy/core/trace"
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

func TestRequestTraceByRequestRouteReturnsScopedCandidatesAndValidatesAdminHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine, _ := requestTraceRouter(t, false)
	now := time.Now().UTC()
	require.NoError(t, model.LogDB.Create(&[]model.RequestTraceSpan{
		{SpanID: requestTraceID(11), TraceID: requestTraceID(21), GroupID: "group-one", RequestID: "duplicate", Service: requesttrace.ServiceAIProxy, Stage: requesttrace.StageRequest, Status: requesttrace.StatusSuccess, StartedAt: now, UpdatedAt: now},
		{SpanID: requestTraceID(12), TraceID: requestTraceID(22), GroupID: "group-one", RequestID: "duplicate", Service: requesttrace.ServiceAIProxy, Stage: requesttrace.StageRequest, Status: requesttrace.StatusError, StartedAt: now, UpdatedAt: now},
		{SpanID: requestTraceID(13), TraceID: requestTraceID(23), GroupID: "group-two", RequestID: "duplicate", Service: requesttrace.ServiceAIProxy, Stage: requesttrace.StageRequest, Status: requesttrace.StatusSuccess, StartedAt: now, UpdatedAt: now},
	}).Error)

	for _, tc := range []struct {
		name, path, header string
		want               int
	}{
		{"missing credentials", "/api/trace/group-one/by-request/duplicate", "", http.StatusUnauthorized},
		{"ordinary key", "/api/trace/group-one/by-request/duplicate", "Bearer client-key", http.StatusUnauthorized},
		{"query-only key", "/api/trace/group-one/by-request/duplicate?key=admin-key", "", http.StatusUnauthorized},
		{"invalid group", "/api/trace/%01/by-request/duplicate", "Bearer admin-key", http.StatusBadRequest},
		{"invalid request", "/api/trace/group-one/by-request/%01", "Bearer admin-key", http.StatusBadRequest},
		{"invalid cursor", "/api/trace/group-one/by-request/duplicate?after=bad", "Bearer admin-key", http.StatusBadRequest},
		{"invalid limit", "/api/trace/group-one/by-request/duplicate?limit=101", "Bearer admin-key", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, req)
			require.Equal(t, tc.want, response.Code)
		})
	}

	req := httptest.NewRequest(http.MethodGet, "/api/trace/group-one/by-request/duplicate?limit=1", nil)
	req.Header.Set("Authorization", "Bearer admin-key")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, req)
	require.Equal(t, http.StatusOK, response.Code)
	var payload struct {
		Data model.TraceRequestPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Items, 1)
	require.Equal(t, requestTraceID(21), payload.Data.Items[0].TraceID)
	require.Equal(t, requestTraceID(11), payload.Data.NextCursor)

	req = httptest.NewRequest(http.MethodGet, "/api/trace/group-one/by-request/duplicate?after="+payload.Data.NextCursor+"&limit=1", nil)
	req.Header.Set("Authorization", "Bearer admin-key")
	response = httptest.NewRecorder()
	engine.ServeHTTP(response, req)
	require.Equal(t, http.StatusOK, response.Code)
	payload = struct {
		Data model.TraceRequestPage `json:"data"`
	}{}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Items, 1)
	require.Equal(t, requestTraceID(22), payload.Data.Items[0].TraceID)
	require.Empty(t, payload.Data.NextCursor)
}

func TestRequestTraceRouteReportsPersistedTruncationWithoutPaginationCursor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine, traceID := requestTraceRouter(t, false)
	now := time.Now().UTC()
	require.NoError(t, model.LogDB.Create(&model.RequestTraceHead{TraceID: traceID, Service: requesttrace.ServiceAIProxy, GroupID: "group-one", Truncated: true, UpdatedAt: now}).Error)
	require.NoError(t, model.LogDB.Create(&model.RequestTraceSpan{SpanID: requestTraceID(31), TraceID: traceID, GroupID: "group-one", RequestID: "request", Service: requesttrace.ServiceAIProxy, Stage: requesttrace.StageRequest, Status: requesttrace.StatusSuccess, StartedAt: now, UpdatedAt: now}).Error)
	req := httptest.NewRequest(http.MethodGet, "/api/trace/group-one/"+traceID+"?service=aiproxy", nil)
	req.Header.Set("Authorization", "Bearer admin-key")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, req)
	require.Equal(t, http.StatusOK, response.Code)
	var payload struct {
		Data struct {
			Truncated  bool   `json:"truncated"`
			NextCursor string `json:"next_cursor"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	require.True(t, payload.Data.Truncated)
	require.Empty(t, payload.Data.NextCursor)
}

func TestTraceHealthRequiresAdminHeaderAndUsesSafeSnakeCaseProjection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousAdminKey := config.AdminKey
	config.AdminKey = "admin-key"
	t.Cleanup(func() { config.AdminKey = previousAdminKey })
	restore := trace.Install(trace.Start(context.Background(), nil, trace.Options{Enabled: true}))
	t.Cleanup(restore)
	engine := gin.New()
	SetAPIRouter(engine)

	for _, tc := range []struct {
		path, header string
		want         int
	}{
		{"/api/trace-health", "", http.StatusUnauthorized},
		{"/api/trace-health?key=admin-key", "", http.StatusUnauthorized},
		{"/api/trace-health", "Bearer client-key", http.StatusUnauthorized},
		{"/api/trace-health", "Bearer admin-key", http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, req)
		require.Equal(t, tc.want, response.Code)
		if tc.want == http.StatusOK {
			require.Contains(t, response.Body.String(), `"initialization_failed":true`)
			require.Contains(t, response.Body.String(), `"write_errors":0`)
			require.NotContains(t, response.Body.String(), "WriteErrors")
			require.NotContains(t, response.Body.String(), "postgres")
		}
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
