package controller

import (
	"net/http"
	"strconv"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/trace"
)

const defaultRequestTraceLimit = 50

// safeTracePage is the only trace response shape exposed by the admin API.
// It deliberately has no request or response payload fields.
type safeTracePage struct {
	Items      []safeTraceSpan `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty"`
	Truncated  bool            `json:"truncated"`
}

type safeTraceSpan struct {
	Version      int                     `json:"version"`
	TraceID      string                  `json:"trace_id"`
	SpanID       string                  `json:"span_id"`
	ParentSpanID string                  `json:"parent_span_id,omitempty"`
	RequestID    string                  `json:"request_id"`
	GroupID      string                  `json:"group_id"`
	Service      requesttrace.Service    `json:"service"`
	Stage        requesttrace.Stage      `json:"stage"`
	Status       requesttrace.Status     `json:"status"`
	StartedAt    time.Time               `json:"started_at"`
	EndedAt      *time.Time              `json:"ended_at,omitempty"`
	DurationMS   *float64                `json:"duration_ms,omitempty"`
	Revision     int                     `json:"revision"`
	Truncated    bool                    `json:"truncated"`
	Attributes   requesttrace.Attributes `json:"attributes"`
}

// GetRequestTrace returns a bounded, safe projection of a persisted trace.
func GetRequestTrace(c *gin.Context) {
	// AdminAuth accepts its legacy query-key fallback for existing routes. This
	// endpoint must require a credential supplied in an HTTP header instead.
	if c.GetHeader("Authorization") == "" {
		middleware.ErrorResponse(c, http.StatusUnauthorized, "unauthorized")
		return
	}

	query, ok := parseTraceQuery(c)
	if !ok {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid trace query")
		return
	}
	if model.LogDB == nil {
		middleware.ErrorResponse(c, http.StatusServiceUnavailable, "trace_unavailable")
		return
	}

	page, err := model.NewTraceStore(model.LogDB).List(c.Request.Context(), query)
	if err != nil {
		middleware.ErrorResponse(c, http.StatusServiceUnavailable, "trace_unavailable")
		return
	}
	middleware.SuccessResponse(c, projectTracePage(page))
}

func GetRequestTracesByRequest(c *gin.Context) {
	if c.GetHeader("Authorization") == "" {
		middleware.ErrorResponse(c, http.StatusUnauthorized, "unauthorized")
		return
	}
	query, ok := parseTraceRequestQuery(c)
	if !ok {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid trace request query")
		return
	}
	if model.LogDB == nil {
		middleware.ErrorResponse(c, http.StatusServiceUnavailable, "trace_unavailable")
		return
	}
	page, err := model.NewTraceStore(model.LogDB).FindRequests(c.Request.Context(), query)
	if err != nil {
		middleware.ErrorResponse(c, http.StatusServiceUnavailable, "trace_unavailable")
		return
	}
	middleware.SuccessResponse(c, page)
}

func parseTraceRequestQuery(c *gin.Context) (model.TraceRequestQuery, bool) {
	groupID, requestID := c.Param("group"), c.Param("request_id")
	if !validTraceGroup(groupID) || !validTraceRequestID(requestID) {
		return model.TraceRequestQuery{}, false
	}
	after := c.Query("after")
	if after != "" && !validTraceID(after) {
		return model.TraceRequestQuery{}, false
	}
	limit := defaultRequestTraceLimit
	if raw, present := c.GetQuery("limit"); present {
		parsed, err := strconv.ParseUint(raw, 10, 8)
		if err != nil || parsed < 1 || parsed > 100 {
			return model.TraceRequestQuery{}, false
		}
		limit = int(parsed)
	}
	return model.TraceRequestQuery{GroupID: groupID, RequestID: requestID, AfterSpanID: after, Limit: limit}, true
}

func validTraceRequestID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

type safeTraceHealth struct {
	Enabled              bool                  `json:"enabled"`
	Ready                bool                  `json:"ready"`
	Writer               safeTraceWriterHealth `json:"writer"`
	CleanupErrors        uint64                `json:"cleanup_errors"`
	InitializationFailed bool                  `json:"initialization_failed"`
}

type safeTraceWriterHealth struct {
	Accepted    uint64 `json:"accepted"`
	Persisted   uint64 `json:"persisted"`
	Rejected    uint64 `json:"rejected"`
	Dropped     uint64 `json:"dropped"`
	WriteErrors uint64 `json:"write_errors"`
}

func GetTraceHealth(c *gin.Context) {
	if c.GetHeader("Authorization") == "" {
		middleware.ErrorResponse(c, http.StatusUnauthorized, "unauthorized")
		return
	}
	health := trace.Current().Health()
	middleware.SuccessResponse(c, safeTraceHealth{
		Enabled: health.Enabled, Ready: health.Ready, CleanupErrors: health.CleanupErrors, InitializationFailed: health.InitializationFailed,
		Writer: safeTraceWriterHealth{Accepted: health.Writer.Accepted, Persisted: health.Writer.Persisted, Rejected: health.Writer.Rejected, Dropped: health.Writer.Dropped, WriteErrors: health.Writer.WriteErrors},
	})
}

func parseTraceQuery(c *gin.Context) (model.TraceQuery, bool) {
	groupID := c.Param("group")
	traceID := c.Param("trace_id")
	if !validTraceGroup(groupID) || !validTraceID(traceID) {
		return model.TraceQuery{}, false
	}

	service := requesttrace.Service(c.Query("service"))
	if service != requesttrace.ServiceAIProxy && service != requesttrace.ServiceApp {
		return model.TraceQuery{}, false
	}

	after := c.Query("after")
	if after != "" && !validTraceID(after) {
		return model.TraceQuery{}, false
	}

	limit := defaultRequestTraceLimit
	if rawLimit, present := c.GetQuery("limit"); present {
		parsedLimit, err := strconv.ParseUint(rawLimit, 10, 8)
		if err != nil || parsedLimit < 1 || parsedLimit > 100 {
			return model.TraceQuery{}, false
		}
		limit = int(parsedLimit)
	}

	return model.TraceQuery{
		GroupID: groupID, TraceID: traceID, Service: service, AfterSpanID: after, Limit: limit,
	}, true
}

func validTraceGroup(groupID string) bool {
	if groupID == "" || len(groupID) > 64 {
		return false
	}
	for _, r := range groupID {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validTraceID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func projectTracePage(page model.TracePage) safeTracePage {
	projected := safeTracePage{
		Items: make([]safeTraceSpan, 0, len(page.Items)), NextCursor: page.NextCursor, Truncated: page.Truncated,
	}
	for _, span := range page.Items {
		projected.Items = append(projected.Items, safeTraceSpan{
			Version: span.Version, TraceID: span.TraceID, SpanID: span.SpanID, ParentSpanID: span.ParentSpanID,
			RequestID: span.RequestID, GroupID: span.GroupID, Service: span.Service, Stage: span.Stage,
			Status: span.Status, StartedAt: span.StartedAt, EndedAt: span.EndedAt, DurationMS: span.DurationMS,
			Revision: span.Revision, Truncated: span.Truncated, Attributes: span.Attributes,
		})
	}
	return projected
}
