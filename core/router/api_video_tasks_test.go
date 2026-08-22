package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/stretchr/testify/require"
)

func TestVideoTaskRouteIsAdminProtected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousAdminKey := config.AdminKey
	config.AdminKey = "test-admin-key"
	t.Cleanup(func() {
		config.AdminKey = previousAdminKey
	})

	engine := gin.New()
	SetAPIRouter(engine)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/video_tasks/group-a?page=1&per_page=20",
		nil,
	)

	engine.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	require.Contains(t, recorder.Body.String(), "unauthorized")
}
