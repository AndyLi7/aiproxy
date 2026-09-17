package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

func TestMeasuredEffectiveModelRejectedBeforeInference(t *testing.T) {
	for _, kind := range []mode.Mode{mode.ChatCompletions, mode.ImagesGenerations} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/test", nil)
		mc := model.ModelConfig{
			Model: "model",
			Type:  kind,
			Price: model.Price{
				ImageBilling: &model.ImageBillingPolicy{Version: 1, Scenario: "generation"},
			},
		}

		valid := validateEffectiveRelayModel(c, kind, mc, "model")
		if kind == mode.ChatCompletions {
			require.False(t, valid)
			require.True(t, c.IsAborted())
			require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
		} else {
			require.True(t, valid)
			require.False(t, c.IsAborted())
		}
	}
}
