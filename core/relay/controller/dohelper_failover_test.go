package controller

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/meta"
)

func TestDoRequestPreservesEarlierCallUncertainty(t *testing.T) {
	for _, prior := range []string{"none", "response", "uncertain"} {
		t.Run(prior, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Set(failover.AttemptContextKey, &failover.Attempt{})
			req := httptest.NewRequest(http.MethodPost, "http://provider.test", nil)
			m := &meta.Meta{}
			if prior != "none" {
				a := testAdaptor{doRequest: func(*meta.Meta, adaptor.Store, *gin.Context, *http.Request) (*http.Response, error) {
					if prior == "response" {
						return &http.Response{StatusCode: 200}, nil
					}
					return nil, context.DeadlineExceeded
				}}
				doRequest(a, c, m, nil, req)
			}
			a := testAdaptor{doRequest: func(_ *meta.Meta, _ adaptor.Store, _ *gin.Context, r *http.Request) (*http.Response, error) {
				httptrace.ContextClientTrace(r.Context()).ConnectStart("tcp", "provider.test:80")
				return nil, &net.OpError{Op: "dial", Err: errors.New("refused")}
			}}
			_, err := doRequest(a, c, m, nil, req)
			got := failover.FromError(err)
			want := failover.Unknown
			if prior == "none" {
				want = failover.NotAccepted
			}
			if got.Acceptance != want {
				t.Fatalf("acceptance=%s want=%s", got.Acceptance, want)
			}
		})
	}
}
