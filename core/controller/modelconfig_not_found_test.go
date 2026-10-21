package controller_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/controller"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestModelConfigReadHTTP(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "config.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ModelConfig{}))

	oldDB, oldKey := model.DB, config.AdminKey
	model.DB, config.AdminKey = db, "local-test-only"
	t.Cleanup(func() {
		model.DB, config.AdminKey = oldDB, oldKey
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	})
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(middleware.AdminAuth)
	router.GET("/api/model_config/*model", controller.GetModelConfig)

	call := func(id, auth string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(
			context.Background(),
			"GET",
			"/api/model_config/"+id,
			nil,
		)
		req.Header.Set("Authorization", auth)
		router.ServeHTTP(w, req)

		return w
	}

	const (
		id   = "bytedance/seedream-5-pro::text-to-image"
		auth = "Bearer local-test-only"
	)

	require.Equal(t, http.StatusUnauthorized, call(id, "").Code)

	for range 3 {
		missing := call(id, auth)
		require.Equal(t, http.StatusNotFound, missing.Code)
		require.JSONEq(
			t,
			`{"success":false,"code":"model_config_not_found","message":"model config record not found"}`,
			missing.Body.String(),
		)
	}

	var count int64
	require.NoError(t, db.Model(&model.ModelConfig{}).Count(&count).Error)
	require.Zero(t, count, "a read must not manufacture a model config")
	require.NoError(t, db.Create(&model.ModelConfig{Model: id}).Error)
	existing := call(id, auth)
	require.Equal(t, http.StatusOK, existing.Code)
	require.Contains(t, existing.Body.String(), id)
	// A broken store is still an error, not a missing configuration.
	require.NoError(t, db.Migrator().DropTable(&model.ModelConfig{}))

	failed := call(id, auth)
	require.Equal(t, http.StatusInternalServerError, failed.Code)
	require.NotContains(t, failed.Body.String(), `"code":"model_config_not_found"`)
}
