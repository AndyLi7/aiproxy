//nolint:testpackage // White-box coverage needs the package-private imageTaskHTTPError fixture or implementation.
package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestTaskErrorsHaveActionableMessages(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{404, "task_not_found"}, {503, "channel_unavailable"}, {400, "unsupported_image_execution"}, {409, "request_id_conflict"}, {503, "task_store_unavailable"}} {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		imageTaskHTTPError(c, tc.status, tc.code)

		var body struct {
			Error struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
				Type    string `json:"Type"`
			} `json:"Error"`
		}
		require.NoError(t, json.Unmarshal(r.Body.Bytes(), &body))
		require.Equal(t, tc.code, body.Error.Code)
		require.NotEqual(t, body.Error.Code, body.Error.Message)
		require.NotEmpty(t, body.Error.Type)
	}
}

// Generated clients act on these sentences, so they are part of the contract.
func TestImageTaskRetryGuidanceMessages(t *testing.T) {
	require.Equal(
		t,
		"This X-Request-Id is already used by a different request body or another API key. Retry with the identical body and key, or use a new X-Request-Id for a new generation.",
		imageTaskErrorMessage("request_id_conflict"),
	)
	require.Equal(
		t,
		"No image task with this ID exists for this API key. If your POST may not have reached us (timeout or network error), resend the same body with the same X-Request-Id; it is idempotent and is not charged twice. Do not keep polling an ID that returns 404.",
		imageTaskErrorMessage("task_not_found"),
	)
	// A delivered result may still be awaiting its final charge and counts
	// toward the per-account limit until it is billed (owner decision 2026-10-12).
	require.Equal(
		t,
		"Too many of your tasks are generating or still being billed. Wait a moment for one to finish, then retry with the same X-Request-Id.",
		imageTaskErrorMessage("too_many_active_tasks"),
	)
}
