package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRequestIDMiddlewarePreservesSafeInboundID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		"/v1/videos",
		nil,
	)
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
	c.Request = httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		"/v1/videos",
		nil,
	)
	c.Request.Header.Set(RequestIDHeader, "short")
	SetRequestAt(c, time.Unix(0, 123456789000))

	RequestIDMiddleware(c)

	if got := GetRequestID(c); got == "short" || got == "" {
		t.Fatalf("request id = %q, want generated safe id", got)
	}
}

func TestBillingOperationIDCannotBeChosenByClient(t *testing.T) {
	var previous string
	for range 2 {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
		c.Request.Header.Set(RequestIDHeader, "same-client-trace")
		c.Request.Header.Set("X-Billing-Operation-ID", "same-client-trace")
		RequestIDMiddleware(c)
		id := GetBillingOperationID(c)
		if id == "" || id == "same-client-trace" || id == previous {
			t.Fatal("billing identity must be server owned and distinct per execution")
		}
		if GetBillingOperationID(c) != id {
			t.Fatal("channel retries must reuse billing identity")
		}
		if GetRequestID(c) != "same-client-trace" {
			t.Fatal("correlation id must be preserved")
		}
		previous = id
	}
}
