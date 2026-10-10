package controller

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/relay/adaptor"
	relaycontroller "github.com/labring/aiproxy/core/relay/controller"
	"github.com/labring/aiproxy/core/relay/meta"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRelayHelperAcceptanceSafety(t *testing.T) {
	for _, tc := range []struct {
		name            string
		acceptance      failover.Acceptance
		written, pinned bool
		id              string
		want            bool
	}{
		{"unknown HTTP 503", failover.Unknown, false, false, "", false},
		{"safe connect failure", failover.NotAccepted, false, false, "", true},
		{"accepted", failover.Accepted, false, false, "", false},
		{"task receipt", failover.NotAccepted, false, false, "task-1", false},
		{"stream written", failover.NotAccepted, true, false, "", false},
		{"pinned", failover.NotAccepted, false, true, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			c.Set("failover_pinned", tc.pinned)
			if tc.written {
				c.Writer.WriteString("data: hello\n\n")
			}
			m := &meta.Meta{}
			_, retry := RelayHelper(c, m, func(*gin.Context, *meta.Meta) *relaycontroller.HandleResult {
				return &relaycontroller.HandleResult{UpstreamID: tc.id, Error: adaptor.WithFailover(relaymodel.WrapperOpenAIErrorWithMessage("failed", "failed", 503), failover.Failure{Acceptance: tc.acceptance, Class: failover.Transient})}
			})
			if retry != tc.want {
				t.Fatalf("retry=%v", retry)
			}
			var records []map[string]any
			if err := json.Unmarshal([]byte(middleware.GetRequestMetadata(c)["channel_failover_attempts"]), &records); err != nil || len(records) != 1 {
				t.Fatalf("missing audit: %v", err)
			}
		})
	}
}

func TestRelayHelperResetsStickyEvidenceBetweenChannels(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	m := &meta.Meta{}
	safe := failover.Failure{Acceptance: failover.NotAccepted, Class: failover.Transient}
	for _, prior := range []bool{true, false} {
		_, retry := RelayHelper(c, m, func(c *gin.Context, _ *meta.Meta) *relaycontroller.HandleResult {
			value, _ := c.Get(failover.AttemptContextKey)
			evidence := value.(*failover.Attempt)
			if prior {
				evidence.BeginCall()
				evidence.Observe(failover.Failure{Acceptance: failover.Accepted})
			}
			evidence.BeginCall()
			return &relaycontroller.HandleResult{Error: adaptor.WithFailover(relaymodel.WrapperOpenAIErrorWithMessage("failed", "failed", 503), safe)}
		})
		if retry == prior {
			t.Fatalf("prior=%v retry=%v", prior, retry)
		}
	}
}
