package router

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestUsageRoutesAreAdminProtected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousAdminKey := config.AdminKey
	config.AdminKey = "usage-admin-key"

	t.Cleanup(func() { config.AdminKey = previousAdminKey })

	database, err := model.OpenSQLite(filepath.Join(t.TempDir(), "router-usage.db"))
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.Log{}))

	previousLogDB := model.LogDB
	model.LogDB = database

	t.Cleanup(func() {
		model.LogDB = previousLogDB

		if sqlDB, sqlErr := database.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	})

	engine := gin.New()
	SetAPIRouter(engine)

	// The last 30 days: windows may start at most 400 days back.
	now := time.Now()
	window := "start=" + strconv.FormatInt(now.Add(-30*24*time.Hour).UnixMilli(), 10) +
		"&end=" + strconv.FormatInt(now.UnixMilli(), 10)

	for _, path := range []string{
		"/api/usage/g/calls?" + window,
		"/api/usage/g/keys?token_ids=1&" + window,
	} {
		for _, key := range []string{"", "Bearer wrong-key"} {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
			if key != "" {
				request.Header.Set("Authorization", key)
			}

			w := httptest.NewRecorder()
			engine.ServeHTTP(w, request)
			require.Equal(t, http.StatusUnauthorized, w.Code, path)
		}

		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+config.AdminKey)

		w := httptest.NewRecorder()
		engine.ServeHTTP(w, request)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
}
