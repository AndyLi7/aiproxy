package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/trace"
	"github.com/stretchr/testify/require"
)

func TestV1RequestTraceRunsBeforeAuthenticationOnlyForSupportedRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "relay_request_trace.db"))
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = oldLogDB })
	runtime := trace.Start(t.Context(), db, trace.Options{Enabled: true})
	require.True(t, runtime.Health().Ready)
	restore := trace.Install(runtime)
	t.Cleanup(restore)

	engine := gin.New()
	engine.Use(middleware.RequestIDMiddleware)
	SetRelayRouter(engine)

	for range 2 {
		request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
		request.Header.Set(middleware.RequestIDHeader, "same-external-request")
		request.Header.Set("traceparent", "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01")
		request.Header.Set("X-Trace-ID", "cccccccccccccccccccccccccccccccc")
		request.Header.Set("X-Trace-Source", "untrusted-client")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		require.Equal(t, http.StatusUnauthorized, response.Code)
	}

	unsupported := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	unsupported.Header.Set(middleware.RequestIDHeader, "same-external-request")
	engine.ServeHTTP(httptest.NewRecorder(), unsupported)

	require.NoError(t, runtime.Close(context.Background()))
	var heads []model.RequestTraceHead
	require.NoError(t, db.Order("trace_id").Find(&heads).Error)
	require.Len(t, heads, 2)
	require.NotEqual(t, heads[0].TraceID, heads[1].TraceID)
	for _, head := range heads {
		require.Empty(t, head.GroupID)
		require.NotEqual(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", head.TraceID)
		require.NotEqual(t, "cccccccccccccccccccccccccccccccc", head.TraceID)
	}

	var spans []model.RequestTraceSpan
	require.NoError(t, db.Order("trace_id, stage, revision").Find(&spans).Error)
	require.Len(t, spans, 4)
	for _, span := range spans {
		require.Equal(t, "same-external-request", span.RequestID)
		require.Empty(t, span.GroupID)
		require.Empty(t, span.Attributes)
		require.Contains(t, []requesttrace.Stage{
			requesttrace.StageRequest,
			requesttrace.StageAuthentication,
		}, span.Stage)
		require.Equal(t, requesttrace.StatusError, span.Status)
	}
}
