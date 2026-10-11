package router

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestLastAPIRequestRouteIsAdminProtected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousAdminKey := config.AdminKey
	config.AdminKey = "test-admin-key"

	t.Cleanup(func() { config.AdminKey = previousAdminKey })

	database, err := model.OpenSQLite(filepath.Join(t.TempDir(), "router-last-api-request.db"))
	require.NoError(t, err)

	previousLogDB := model.LogDB
	model.LogDB = database

	t.Cleanup(func() {
		model.LogDB = previousLogDB

		if sqlDB, sqlErr := database.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, database.AutoMigrate(&model.Log{}, &model.RequestDetail{}))

	engine := gin.New()
	SetAPIRouter(engine)

	call := func(key string) *httptest.ResponseRecorder {
		request := httptest.NewRequestWithContext(
			t.Context(),
			http.MethodPost,
			"/api/logs/last_api_request",
			strings.NewReader(`{"groups":["u_a"]}`),
		)
		request.Header.Set("Content-Type", "application/json")

		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}

		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)

		return recorder
	}

	for _, key := range []string{"", "wrong-key"} {
		recorder := call(key)
		require.Equal(t, http.StatusUnauthorized, recorder.Code)
		require.Contains(t, recorder.Body.String(), "unauthorized")
	}

	recorder := call(config.AdminKey)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.JSONEq(t, `{"success":true,"data":{"items":[]}}`, recorder.Body.String())
}
