package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/labring/aiproxy/core/controller"
	"github.com/labring/aiproxy/core/model"
	requesttraceruntime "github.com/labring/aiproxy/core/trace"
	"github.com/stretchr/testify/require"
)

func TestRequestTraceStartupPersistsToLogDatabaseUsedByAdminEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	primaryDB, err := model.OpenSQLite(filepath.Join(t.TempDir(), "primary.db"))
	require.NoError(t, err)
	logDB, err := model.OpenSQLite(filepath.Join(t.TempDir(), "log.db"))
	require.NoError(t, err)
	primarySQL, err := primaryDB.DB()
	require.NoError(t, err)
	logSQL, err := logDB.DB()
	require.NoError(t, err)
	oldPrimary, oldLog := model.DB, model.LogDB
	model.DB, model.LogDB = primaryDB, logDB
	t.Cleanup(func() {
		model.DB, model.LogDB = oldPrimary, oldLog
		require.NoError(t, primarySQL.Close())
		require.NoError(t, logSQL.Close())
	})

	runtime := startRequestTraceRuntime(context.Background(), requesttraceruntime.Options{Enabled: true})
	runtimeClosed := false
	t.Cleanup(func() {
		if !runtimeClosed {
			require.NoError(t, runtime.Close(context.Background()))
		}
	})
	require.True(t, runtime.Health().Ready)
	session := runtime.NewRequest("startup-request")
	require.True(t, session.BindGroup("startup-group"))
	require.True(t, session.Finish(requesttrace.StatusSuccess))
	traceID := session.TraceID()
	require.NoError(t, runtime.Close(context.Background()))
	runtimeClosed = true

	require.False(t, primaryDB.Migrator().HasTable(&model.RequestTraceSpan{}))
	require.False(t, primaryDB.Migrator().HasTable(&model.RequestTraceHead{}))
	require.True(t, logDB.Migrator().HasTable(&model.RequestTraceSpan{}))
	require.True(t, logDB.Migrator().HasTable(&model.RequestTraceHead{}))
	for _, table := range []any{&model.RequestTraceSpan{}, &model.RequestTraceHead{}} {
		var count int64
		require.NoError(t, logDB.Model(table).Count(&count).Error)
		require.Equal(t, int64(1), count)
	}

	engine := gin.New()
	engine.GET("/trace/:group/by-request/:request_id", controller.GetRequestTracesByRequest)
	engine.GET("/trace/:group/:trace_id", controller.GetRequestTrace)
	for _, path := range []string{
		fmt.Sprintf("/trace/startup-group/%s?service=aiproxy", traceID),
		"/trace/startup-group/by-request/startup-request",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer admin")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var payload struct {
			Success bool `json:"success"`
			Data    struct {
				Items []json.RawMessage `json:"items"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
		require.True(t, payload.Success)
		require.Len(t, payload.Data.Items, 1)
		require.Contains(t, string(payload.Data.Items[0]), traceID)
	}
}
