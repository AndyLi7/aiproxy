package controller

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
)

func TestTaskErrorsHaveActionableMessages(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{404, "task_not_found"}, {503, "channel_unavailable"}, {400, "unsupported_image_execution"}, {409, "request_id_conflict"}, {503, "task_store_unavailable"}} {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest("GET", "/", nil)
		imageTaskHTTPError(c, tc.status, tc.code)
		var body struct {
			Error struct{ Code, Message, Type string }
		}
		require.NoError(t, json.Unmarshal(r.Body.Bytes(), &body))
		require.Equal(t, tc.code, body.Error.Code)
		require.NotEqual(t, body.Error.Code, body.Error.Message)
		require.NotEmpty(t, body.Error.Type)
	}
}
