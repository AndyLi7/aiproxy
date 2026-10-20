package controller_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/controller"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/assert"
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

	const (
		bodyLimit = 16 * 1024
		extraJSON = `{"quota":999}`
	)

	for index, tc := range []struct {
		name   string
		body   string
		status int
	}{
		{"below-limit", "{}" + strings.Repeat(" ", bodyLimit-3), http.StatusOK},
		{"exact-limit", "{}" + strings.Repeat(" ", bodyLimit-2), http.StatusOK},
		{"one-byte-over", "{}" + strings.Repeat(" ", bodyLimit-1), http.StatusRequestEntityTooLarge},
		{"oversized-whitespace", "{}" + strings.Repeat(" ", 17000), http.StatusRequestEntityTooLarge},
		{"hidden-trailing-json", "{}" + strings.Repeat(" ", 16383) + extraJSON, http.StatusRequestEntityTooLarge},
		{"trailing-json-at-limit", "{}" + strings.Repeat(" ", bodyLimit-2-len(extraJSON)) + extraJSON, http.StatusBadRequest},
		{"short-trailing-json", "{} {}", http.StatusBadRequest},
		{"oversized-string", `{"available_sets":["` + strings.Repeat("x", 17000) + `"]}`, http.StatusRequestEntityTooLarge},
	} {
		for _, unknownLength := range []bool{false, true} {
			t.Run(tc.name+"/unknown-length="+strconv.FormatBool(unknownLength), func(t *testing.T) {
				id := "body-boundary-" + strconv.Itoa(
					index,
				) + "-" + strconv.FormatBool(
					unknownLength,
				)
				request := httptest.NewRequestWithContext(
					t.Context(),
					http.MethodPost,
					"/api/group/"+id+"/ensure",
					strings.NewReader(tc.body),
				)
				request.Header.Set("Authorization", auth)
				request.Header.Set("Content-Type", "application/json")

				if unknownLength {
					request.ContentLength = -1
				}

				var before int64
				require.NoError(t, db.Model(&model.Group{}).Count(&before).Error)

				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)

				var after, persisted int64
				require.NoError(t, db.Model(&model.Group{}).Count(&after).Error)
				require.NoError(
					t,
					db.Model(&model.Group{}).Where("id = ?", id).Count(&persisted).Error,
				)

				if tc.status == http.StatusOK {
					assert.EqualValues(t, 1, persisted)
					assert.Equal(t, before+1, after)
				} else {
					assert.Zero(t, persisted, "rejected body must not create a group")
					assert.Equal(t, before, after, "rejected body must not write to groups")
				}

				assert.Equal(t, tc.status, response.Code, "body bytes=%d", len(tc.body))
			})
		}
	}

	require.NoError(t, db.Migrator().DropTable(&model.Group{}))
	require.Equal(t, http.StatusInternalServerError, call("GET", "/api/group/demo", "", auth).Code)
	require.Equal(
		t,
		http.StatusInternalServerError,
		call("POST", "/api/group/demo/ensure", "{}", auth).Code,
	)
}
