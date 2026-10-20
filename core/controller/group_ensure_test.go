package controller_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/controller"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestGroupEnsureHTTP(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "ensure.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Group{}))

	oldDB, oldLogDB, oldKey := model.DB, model.LogDB, config.AdminKey
	model.DB, model.LogDB, config.AdminKey = db, db, "local-test-only"
	t.Cleanup(func() {
		model.DB, model.LogDB, config.AdminKey = oldDB, oldLogDB, oldKey
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	})
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(middleware.AdminAuth)
	router.GET("/api/group/:group", controller.GetGroup)
	router.POST("/api/group/:group/ensure", controller.EnsureGroup)
	router.POST("/api/group/:group", controller.CreateGroup)

	call := func(method, path, body, auth string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(
			context.Background(),
			method,
			path,
			strings.NewReader(body),
		)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", auth)
		router.ServeHTTP(w, req)

		return w
	}

	const auth = "Bearer local-test-only"

	require.Equal(t, http.StatusUnauthorized, call("POST", "/api/group/demo/ensure", "{}", "").Code)
	missing := call("GET", "/api/group/demo", "", auth)
	require.Equal(t, http.StatusNotFound, missing.Code)
	require.Contains(t, missing.Body.String(), `"code":"group_not_found"`)

	first := call(
		"POST",
		"/api/group/demo/ensure",
		`{"status":3,"available_sets":["private"]}`,
		auth,
	)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())

	again := call(
		"POST",
		"/api/group/demo/ensure",
		`{"status":1,"available_sets":["public"]}`,
		auth,
	)
	require.Equal(t, http.StatusOK, again.Code)

	var payload struct {
		Data struct {
			Status        int      `json:"status"`
			AvailableSets []string `json:"available_sets"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(again.Body.Bytes(), &payload))
	require.Equal(t, 3, payload.Data.Status)
	require.Equal(t, []string{"private"}, payload.Data.AvailableSets)
	require.Equal(
		t,
		http.StatusInternalServerError,
		call("POST", "/api/group/demo", "{}", auth).Code,
	)

	for _, body := range []string{`null`, `{"status":99}`, `{"quota":100}`, `{`, `{} {}`, `{"available_sets":"all"}`} {
		require.Equal(
			t,
			http.StatusBadRequest,
			call("POST", "/api/group/new/ensure", body, auth).Code,
			body,
		)
	}

	require.NoError(t, db.Migrator().DropTable(&model.Group{}))
	require.Equal(t, http.StatusInternalServerError, call("GET", "/api/group/demo", "", auth).Code)
	require.Equal(
		t,
		http.StatusInternalServerError,
		call("POST", "/api/group/demo/ensure", "{}", auth).Code,
	)
}
