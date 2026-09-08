//nolint:testpackage
package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/labring/aiproxy/core/trace"
	"github.com/stretchr/testify/require"
)

func TestRequestTraceMiddlewareAcceptsOnlySupportedB1Routes(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodPost, "/v1/images/generations", true},
		{http.MethodPost, "/v1/images/edits", true},
		{http.MethodPost, "/v1/video/generations/jobs", true},
		{http.MethodGet, "/v1/video/generations/jobs/job-1", true},
		{http.MethodGet, "/v1/video/generations/job-1/content/video", true},
		{http.MethodPost, "/v1/videos", true},
		{http.MethodGet, "/v1/videos/video-1", true},
		{http.MethodGet, "/v1/videos/video-1/content", true},
		{http.MethodDelete, "/v1/videos/video-1", true},
		{http.MethodGet, "/v1/models", false},
		{http.MethodPost, "/v1/images/variations", false},
		{http.MethodPost, "/v1/videos/edits", false},
		{http.MethodPost, "/v1/videos/extensions", false},
		{http.MethodPost, "/v1/videos/video-1/remix", false},
		{http.MethodPost, "/api/v1/services/aigc/video-generation/video-synthesis", false},
		{http.MethodGet, "/v1/videos", false},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			require.Equal(t, tt.want, isRequestTraceRoute(tt.method, tt.path))
		})
	}
}

func TestRequestTraceHelpersBindTrustedGroupAndCreateStages(t *testing.T) {
	var spans []requesttrace.Span
	session := requesttrace.NewSession("request-id", func(span requesttrace.Span) bool {
		spans = append(spans, span)
		return true
	})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(requestTraceSessionKey, session)

	require.True(t, BindRequestTraceGroup(c, "server-group"))
	require.False(t, BindRequestTraceGroup(c, "other-group"))
	handle := BeginRequestTraceStage(c, requesttrace.StageAuthentication, requesttrace.Attributes{})
	require.NotEmpty(t, handle.SpanID())
	require.True(t, handle.Finish(requesttrace.StatusSuccess))
	require.Equal(t, 1, NextRequestTraceAttempt(c))
	require.Equal(t, 2, NextRequestTraceAttempt(c))
	require.True(t, session.Finish(requesttrace.StatusSuccess))

	require.Len(t, spans, 4)
	for _, span := range spans {
		require.Equal(t, "server-group", span.GroupID)
	}
}

func TestRequestTraceOutcomeDistinguishesHTTPAndContextFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		err    error
		want   requesttrace.Status
	}{
		{"success", http.StatusOK, nil, requesttrace.StatusSuccess},
		{"http error", http.StatusBadRequest, nil, requesttrace.StatusError},
		{"cancelled", http.StatusOK, context.Canceled, requesttrace.StatusCancelled},
		{"timeout", http.StatusOK, context.DeadlineExceeded, requesttrace.StatusTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.err != nil {
				var cancel context.CancelFunc
				if tt.err == context.DeadlineExceeded {
					ctx, cancel = context.WithDeadline(ctx, time.Unix(1, 0))
				} else {
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				defer cancel()
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
			c.Status(tt.status)
			require.Equal(t, tt.want, requestTraceOutcome(c))
		})
	}
}

func TestRequestTraceDisabledHelpersAreNilSafe(t *testing.T) {
	restore := trace.Install(nil)
	t.Cleanup(restore)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	RequestTraceMiddleware()(c)
	require.False(t, BindRequestTraceGroup(c, "group"))
	require.Empty(t, BeginRequestTraceStage(c, requesttrace.StageAuthentication, requesttrace.Attributes{}).SpanID())
	require.Zero(t, NextRequestTraceAttempt(c))
}

func TestTokenAuthTracesFailureWithoutTrustingExternalOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var spans []requesttrace.Span
	session := requesttrace.NewSession("request-auth-failure", func(span requesttrace.Span) bool {
		spans = append(spans, span)
		return true
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	c.Request.Header.Set("Group", "attacker-group")
	c.Request.Header.Set("X-Group-ID", "attacker-group")
	c.Set(requestTraceSessionKey, session)

	TokenAuth(c)
	require.True(t, session.Finish(requesttrace.StatusError))
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Len(t, spans, 4)
	for _, span := range spans {
		require.Empty(t, span.GroupID)
		require.Empty(t, span.Attributes)
	}
	require.Equal(t, requesttrace.StageAuthentication, spans[2].Stage)
	require.Equal(t, requesttrace.StatusError, spans[3].Status)
}

func TestTokenAuthBindsResolvedGroupBeforeDisabledGroupRejection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = oldRedisEnabled })
	const key = "trace-auth-key"
	const groupID = "trusted-group"
	require.NoError(t, model.CacheSetToken(&model.TokenCache{
		ID: 7, Key: key, Group: groupID, Status: model.TokenStatusEnabled,
	}))
	require.NoError(t, model.CacheSetGroup(&model.GroupCache{ID: groupID, Status: model.GroupStatusDisabled}))
	t.Cleanup(func() {
		require.NoError(t, model.CacheDeleteToken(key))
		require.NoError(t, model.CacheDeleteGroup(groupID))
	})

	var spans []requesttrace.Span
	session := requesttrace.NewSession("request-auth-group", func(span requesttrace.Span) bool {
		spans = append(spans, span)
		return true
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	c.Request.Header.Set("Authorization", "Bearer "+key)
	c.Request.Header.Set("Group", "attacker-group")
	c.Set(requestTraceSessionKey, session)

	TokenAuth(c)
	require.True(t, session.Finish(requesttrace.StatusError))
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Len(t, spans, 4)
	for _, span := range spans {
		require.Equal(t, groupID, span.GroupID)
		require.Empty(t, span.Attributes)
	}
	require.Equal(t, requesttrace.StageAuthentication, spans[2].Stage)
	require.Equal(t, requesttrace.StatusError, spans[3].Status)
}

func TestDistributeTracesBalanceAndFailedModelResolutionWithEmptyAttributes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var spans []requesttrace.Span
	session := requesttrace.NewSession("request-distribute", func(span requesttrace.Span) bool {
		spans = append(spans, span)
		return true
	})
	require.True(t, session.BindGroup("group-routing"))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/images/generations",
		strings.NewReader(`{}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(requestTraceSessionKey, session)
	c.Set(Group, model.GroupCache{ID: "group-routing", Status: model.GroupStatusInternal})
	c.Set(Token, model.TokenCache{})

	distribute(c, mode.ImagesGenerations)
	require.True(t, session.Finish(requesttrace.StatusError))
	require.Equal(t, http.StatusBadRequest, w.Code)

	completed := make(map[requesttrace.Stage]requesttrace.Span)
	for _, span := range spans {
		if span.Status != requesttrace.StatusRunning {
			completed[span.Stage] = span
		}
	}
	require.Equal(t, requesttrace.StatusSuccess, completed[requesttrace.StageBalanceCheck].Status)
	require.Equal(t, requesttrace.StatusError, completed[requesttrace.StageModelResolution].Status)
	require.Empty(t, completed[requesttrace.StageBalanceCheck].Attributes)
	require.Empty(t, completed[requesttrace.StageModelResolution].Attributes)
}
