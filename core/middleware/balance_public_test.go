package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
)

func TestBalanceFailurePublicMessage(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/videos"} {
		for _, explicitMode := range []bool{false, true} {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", path, nil)
			if explicitMode {
				AbortOperationallyWithMode(mode.ChatCompletions, c, model.FailureStageBalance, 403, "group private-user balance not enough", relaymodel.WithType(GroupBalanceNotEnough))
			} else {
				AbortOperationally(c, model.FailureStageBalance, 403, "group private-user balance not enough", relaymodel.WithType(GroupBalanceNotEnough))
			}
			require.Equal(t, 402, w.Code)
			require.Contains(t, w.Body.String(), "Your account balance is insufficient.")
			require.Contains(t, w.Body.String(), "insufficient_quota")
			require.NotContains(t, w.Body.String(), "private-user")
		}
	}
}
