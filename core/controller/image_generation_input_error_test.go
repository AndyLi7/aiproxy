//nolint:testpackage
package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

// A missing or wrongly typed image field is the client's mistake: it must be a
// coded 400, not a generic 500, and the log must carry the same code.
func TestImageGenerationInputErrorsAreCodedClientErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name, body, code, param, message string
	}{
		{"missing prompt", `{"model":"image"}`, "invalid_parameter", "prompt", "prompt is required and must be a non-empty string."},
		{"empty prompt", `{"model":"image","prompt":""}`, "invalid_parameter", "prompt", "prompt is required and must be a non-empty string."},
		{"wrongly typed field", `{"model":"image","prompt":42}`, "invalid_request", "", "The request body must be a valid JSON object with correctly typed fields: prompt, size, quality, style, response_format and user are strings, n is an integer."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(middleware.ModelConfig, model.ModelConfig{Model: "image", Type: mode.ImagesGenerations})

			relay(c, mode.ImagesGenerations, relayController(mode.ImagesGenerations))

			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			var response struct {
				Error struct {
					Code, Message, Type, Param string
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			require.Equal(t, tc.code, response.Error.Code)
			require.Equal(t, tc.param, response.Error.Param)
			require.Equal(t, tc.message, response.Error.Message)
			require.Equal(t, "invalid_request_error", response.Error.Type)

			fields := middleware.OperationalFieldsFromContext(c)
			require.Equal(t, model.FailureStageValidation, fields.FailureStage)
			require.Equal(t, tc.code, fields.ErrorCode)
		})
	}
}
