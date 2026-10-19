//nolint:testpackage // Balance consumer fixtures intentionally set the unexported balance field.
package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
	"github.com/stretchr/testify/require"
)

func TestBalanceFailurePublicMessage(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/videos"} {
		for _, explicitMode := range []bool{false, true} {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)

			c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, nil)
			if explicitMode {
				AbortOperationallyWithMode(
					mode.ChatCompletions,
					c,
					model.FailureStageBalance,
					403,
					"group private-user balance not enough",
					relaymodel.WithType(GroupBalanceNotEnough),
				)
			} else {
				AbortOperationally(
					c,
					model.FailureStageBalance,
					403,
					"group private-user balance not enough",
					relaymodel.WithType(GroupBalanceNotEnough),
				)
			}

			require.Equal(t, 402, w.Code)
			require.Contains(t, w.Body.String(), "Your account balance is insufficient.")
			require.Contains(t, w.Body.String(), "insufficient_quota")
			require.NotContains(t, w.Body.String(), "private-user")
		}
	}
}

// balanceRefusal runs the distributor's minimum-balance refusal for a group
// whose balance the distributor already loaded.
func balanceRefusal(
	t *testing.T,
	path string,
	explicitMode bool,
	groupBalance float64,
) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, nil)
	c.Set(GroupBalance, &GroupBalanceConsumer{Group: "private-user", balance: groupBalance})

	if explicitMode {
		AbortOperationallyWithMode(
			mode.ChatCompletions,
			c,
			model.FailureStageBalance,
			402,
			"group `private-user` balance not enough",
			relaymodel.WithType(GroupBalanceNotEnough),
		)
	} else {
		AbortOperationally(
			c,
			model.FailureStageBalance,
			402,
			"group `private-user` balance not enough",
			relaymodel.WithType(GroupBalanceNotEnough),
		)
	}

	return w
}

// D7: a balance below zero names the amount owed (rounded up to the cent,
// "less than $0.01" below a cent, never "-$0.00"). The status, type and code
// stay 402 insufficient_quota / insufficient_balance; a balance at or above
// zero keeps the generic sentence.
func TestNegativeBalanceRefusalNamesTheAmountOwed(t *testing.T) {
	const advice = "New requests are paused until your balance is back above zero; a top-up covers the amount owed first."
	for _, path := range []string{"/v1/model-tasks", "/v1/images/tasks", "/v1/chat/completions", "/v1beta/models/gemini:generateContent"} {
		for _, explicitMode := range []bool{false, true} {
			for balance, message := range map[float64]string{
				-0.12:   "Your balance is below zero (-$0.12). " + advice,
				-0.0012: "Your balance is below zero (less than $0.01 owed). " + advice,
				0.005:   "Your account balance is insufficient.",
				0:       "Your account balance is insufficient.",
			} {
				w := balanceRefusal(t, path, explicitMode, balance)
				require.Equal(t, 402, w.Code)

				var body struct {
					Error struct {
						Message string `json:"message"`
						Type    string `json:"type"`
						Code    string `json:"code"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
				require.Equal(
					t,
					message,
					body.Error.Message,
					"%s %v %v",
					path,
					explicitMode,
					balance,
				)
				require.Equal(t, "insufficient_quota", body.Error.Type)
				require.Equal(t, "insufficient_balance", body.Error.Code)
				require.NotContains(t, w.Body.String(), "$0.00")
				require.NotContains(t, w.Body.String(), "private-user")
			}
		}
	}
}

// Off /v1/ only the message changes too: the generic body is byte-for-byte
// today's for a balance that is not negative.
func TestBalanceRefusalBodyUnchangedWhenNotNegative(t *testing.T) {
	for _, path := range []string{"/v1/model-tasks", "/v1beta/models/gemini:generateContent"} {
		withBalance := balanceRefusal(t, path, true, 0.004)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, nil)
		AbortOperationallyWithMode(
			mode.ChatCompletions,
			c,
			model.FailureStageBalance,
			402,
			"group `private-user` balance not enough",
			relaymodel.WithType(GroupBalanceNotEnough),
		)
		require.Equal(t, w.Body.String(), withBalance.Body.String())
	}
}
