package middleware

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRequestIDMiddlewarePreservesSafeInboundID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/videos", nil)
	c.Request.Header.Set(RequestIDHeader, "trace_20260812-aiproxy")
	SetRequestAt(c, time.Now())

	RequestIDMiddleware(c)

	if got := GetRequestID(c); got != "trace_20260812-aiproxy" {
		t.Fatalf("request id = %q, want preserved inbound id", got)
	}
}

func TestRequestIDMiddlewareReplacesUnsafeInboundID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/videos", nil)
	c.Request.Header.Set(RequestIDHeader, "short")
	SetRequestAt(c, time.Unix(0, 123456789000))

	RequestIDMiddleware(c)

	if got := GetRequestID(c); got == "short" || got == "" {
		t.Fatalf("request id = %q, want generated safe id", got)
	}
}
