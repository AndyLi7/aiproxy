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

func RequestTraceMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
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

		defer func() {
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
	return requestTraceSession(c).BindGroup(groupID)
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
