package router_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	corerouter "github.com/labring/aiproxy/core/router"
	"github.com/stretchr/testify/require"
)

func TestVideoTaskRouteIsAdminProtected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousAdminKey := config.AdminKey
	config.AdminKey = "test-admin-key"
	t.Cleanup(func() { config.AdminKey = previousAdminKey })

	r := gin.New()
	corerouter.SetAPIRouter(r)
	w := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/video_tasks/group-a?page=1&per_page=20",
		nil,
	)
	r.ServeHTTP(w, request)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}
