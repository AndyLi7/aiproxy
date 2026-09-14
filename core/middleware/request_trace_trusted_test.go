package middleware

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/trace"
	"github.com/stretchr/testify/require"
)

func TestTrustedRequestBindingPersistsAndStripsServiceEnvelope(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "trusted.db"))
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	key := make([]byte, 32)
	_, err = rand.Read(key)
	require.NoError(t, err)
	runtime := trace.Start(context.Background(), db, trace.Options{Enabled: true, TrustedKeys: map[string][]byte{"app-1": key}})
	restore := trace.Install(runtime)
	t.Cleanup(restore)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	wire, err := requesttrace.SignTrustedContext(requesttrace.TrustedClaims{Version: 1, KeyID: "app-1", TraceID: "11111111111111111111111111111111", ParentSpanID: "22222222222222222222222222222222", GroupID: "group-a", RequestID: "request-a", Source: "playground", Method: "POST", Path: "/v1/images/generations", IssuedAt: time.Now().Unix(), Nonce: "33333333333333333333333333333333"}, key)
	require.NoError(t, err)
	engine := gin.New()
	engine.Use(RequestIDMiddleware, RequestTraceMiddleware())
	engine.POST("/v1/images/generations", func(c *gin.Context) {
		require.Empty(t, c.GetHeader("X-Platform-Trace"))
		require.True(t, BindRequestTraceGroup(c, "group-a"))
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	req.Header.Set("X-Request-ID", "request-a")
	req.Header.Set("X-Platform-Trace", wire)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, req)
	require.Equal(t, http.StatusNoContent, response.Code)
	require.NoError(t, runtime.Close(context.Background()))
	var spans []model.RequestTraceSpan
	require.NoError(t, db.Find(&spans).Error)
	require.Len(t, spans, 1)
	require.Equal(t, "11111111111111111111111111111111", spans[0].TraceID)
	require.Equal(t, "22222222222222222222222222222222", spans[0].ParentSpanID)
	require.Equal(t, "group-a", spans[0].GroupID)
}

func TestTaskTraceSubmitAndPollingShareTraceWithSeparateRoots(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "task.db"))
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&model.AsyncUsageInfo{}))
	runtime := trace.Start(context.Background(), db, trace.Options{Enabled: true})
	restore := trace.Install(runtime)
	t.Cleanup(restore)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	engine := gin.New()
	engine.Use(RequestIDMiddleware, RequestTraceMiddleware())
	engine.POST("/v1/videos", func(c *gin.Context) {
		require.True(t, BindRequestTraceGroup(c, "group-a"))
		info := model.AsyncUsageInfo{GroupID: "group-a", TokenID: 7, ChannelID: 9, UpstreamID: "task-a"}
		require.NoError(t, db.Create(&info).Error)
		SaveRequestTraceTask(c, info.ID, "group-a")
		c.Status(http.StatusAccepted)
	})
	engine.GET("/v1/videos/:video_id", func(c *gin.Context) {
		require.True(t, BindRequestTraceGroup(c, "group-a"))
		BindRequestTraceTask(c, "group-a", 7, 9, "task-a")
		c.Status(http.StatusOK)
	})
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		path := "/v1/videos"
		if method == http.MethodGet {
			path += "/task-a"
		}
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("X-Request-ID", method+"-request")
		engine.ServeHTTP(httptest.NewRecorder(), req)
	}
	require.NoError(t, runtime.Close(context.Background()))
	var spans []model.RequestTraceSpan
	require.NoError(t, db.Order("request_id").Find(&spans).Error)
	require.Len(t, spans, 2)
	require.Equal(t, spans[0].TraceID, spans[1].TraceID)
	require.NotEqual(t, spans[0].SpanID, spans[1].SpanID)
	require.Equal(t, spans[1].SpanID, spans[0].ParentSpanID)
}
