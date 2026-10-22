package controller_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/controller"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestGetModelConfig(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "modelconfig.db"))
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	previousDB := model.DB
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		require.NoError(t, pool.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.ModelConfig{}))

	router := gin.New()
	router.GET("/api/model_config/*model", controller.GetModelConfig)
	for _, name := range []string{
		"bytedance/seedream-5-pro",
		"bytedance/seedream-5-lite",
		"bytedance/seedream-5-flash",
		"bytedance/seedream-5-flash-layerize",
	} {
		t.Run(name, func(t *testing.T) {
			for _, path := range []string{name, url.PathEscape(name)} {
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/model_config/"+path, nil))
				require.Equal(t, http.StatusNotFound, w.Code)
				var response middleware.APIResponse
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				require.False(t, response.Success)
				require.Equal(t, "model config not found", response.Message)
			}

			require.NoError(t, db.Create(&model.ModelConfig{Model: name, RPM: 37}).Error)
			for _, path := range []string{name, url.PathEscape(name)} {
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/model_config/"+path, nil))
				require.Equal(t, http.StatusOK, w.Code)
				var response struct {
					Success bool              `json:"success"`
					Data    model.ModelConfig `json:"data"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				require.True(t, response.Success)
				require.Equal(t, name, response.Data.Model)
				require.EqualValues(t, 37, response.Data.RPM)
			}
		})
	}

	t.Run("database error", func(t *testing.T) {
		require.NoError(t, db.Migrator().DropTable(&model.ModelConfig{}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/model_config/bytedance/seedream-5-pro", nil))
		require.Equal(t, http.StatusInternalServerError, w.Code)
		var response middleware.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		require.False(t, response.Success)
		require.Contains(t, response.Message, "no such table")
	})
}
