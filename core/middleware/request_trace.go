package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/labring/aiproxy/core/trace"
)

const requestTraceSessionKey = "request_trace_session"
const requestTraceEnvelopeKey = "request_trace_envelope"
const requestTracePendingOwnerKey = "request_trace_pending_owner"

func RequestTraceMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Service credentials must never be forwarded to an upstream provider.
		wire := c.GetHeader("X-Platform-Trace")
		c.Request.Header.Del("X-Platform-Trace")
		if !isRequestTraceRoute(c.Request.Method, c.Request.URL.Path) {
			c.Next()
			return
		}

		session := trace.Current().NewRequest(GetRequestID(c))
		if session == nil {
			c.Next()
			return
		}
		c.Set(requestTraceSessionKey, session)
		if len(wire) <= 2048 {
			c.Set(requestTraceEnvelopeKey, wire)
		}

		defer func() {
			if group, ok := c.Get(requestTracePendingOwnerKey); ok {
				if owner, ok := group.(string); ok {
					session.BindGroup(owner)
				}
			}
			if recovered := recover(); recovered != nil {
				session.Finish(requesttrace.StatusError)
				panic(recovered)
			}
			session.Finish(requestTraceOutcome(c))
		}()
		c.Next()
	}
}

func BindRequestTraceGroup(c *gin.Context, groupID string) bool {
	if c == nil {
		return false
	}
	if c.Request == nil {
		return requestTraceSession(c).BindGroup(groupID)
	}
	if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodDelete {
		// The distributor will resolve task ownership before flushing identity.
		c.Set(requestTracePendingOwnerKey, groupID)
		c.Set(requestTraceEnvelopeKey, "")
		return requestTraceSession(c) != nil
	}
	wire, _ := c.Get(requestTraceEnvelopeKey)
	c.Set(requestTraceEnvelopeKey, "")
	envelope, _ := wire.(string)
	return trace.Current().BindRequest(c.Request.Context(), requestTraceSession(c), envelope, requesttrace.TrustedRequest{
		GroupID: groupID, RequestID: GetRequestID(c), Method: c.Request.Method, Path: c.Request.URL.Path,
	})
}

func BindRequestTraceTask(c *gin.Context, group string, tokenID, channelID int, taskID string) {
	if c == nil || c.Request == nil {
		return
	}
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodDelete {
		return
	}
	trace.Current().BindTask(c.Request.Context(), requestTraceSession(c), group, tokenID, channelID, taskID)
}

func SaveRequestTraceTask(c *gin.Context, asyncID int, group string) {
	if c == nil || c.Request == nil {
		return
	}
	trace.Current().SaveTask(c.Request.Context(), requestTraceSession(c), asyncID, group)
}

func BeginRequestTraceStage(
	c *gin.Context,
	stage requesttrace.Stage,
	attrs requesttrace.Attributes,
) *requesttrace.Handle {
	return requestTraceSession(c).Begin(stage, attrs)
}

func NextRequestTraceAttempt(c *gin.Context) int {
	return requestTraceSession(c).NextAttempt()
}

func requestTraceSession(c *gin.Context) *requesttrace.Session {
	if c == nil {
		return nil
	}
	session, _ := c.Get(requestTraceSessionKey)
	value, _ := session.(*requesttrace.Session)
	return value
}

func requestTraceOutcome(c *gin.Context) requesttrace.Status {
	if err := c.Request.Context().Err(); errors.Is(err, context.DeadlineExceeded) {
		return requesttrace.StatusTimeout
	} else if err != nil {
		return requesttrace.StatusCancelled
	}
	if c.Writer.Status() >= http.StatusBadRequest {
		return requesttrace.StatusError
	}
	return requesttrace.StatusSuccess
}

func isRequestTraceRoute(method, path string) bool {
	switch method {
	case http.MethodPost:
		return path == "/v1/images/generations" ||
			path == "/v1/images/edits" ||
			path == "/v1/videos"
	case http.MethodGet:
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		return len(parts) == 3 && parts[0] == "v1" && parts[1] == "videos" && parts[2] != "" ||
			len(parts) == 4 && parts[0] == "v1" && parts[1] == "videos" &&
				parts[2] != "" && parts[3] == "content"
	case http.MethodDelete:
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		return len(parts) == 3 && parts[0] == "v1" && parts[1] == "videos" && parts[2] != ""
	default:
		return false
	}
}
