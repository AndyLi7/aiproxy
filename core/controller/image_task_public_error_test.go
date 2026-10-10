package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestImageTaskErrorsUsePublicErrorEnvelope(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		kind   string
	}{
		{400, "unsupported_image_execution", "invalid_request_error"},
		{404, "task_not_found", "not_found_error"},
		{503, "task_store_unavailable", "api_error"},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/images/tasks/example", nil)
		imageTaskHTTPError(c, tc.status, tc.code)
		require.Equal(t, tc.status, w.Code)
		var response struct {
			Error map[string]any `json:"error"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		require.Equal(t, tc.code, response.Error["code"])
		require.Equal(t, tc.kind, response.Error["type"])
		require.Contains(t, response.Error, "param")
		require.NotEmpty(t, response.Error["message"])
	}
}
