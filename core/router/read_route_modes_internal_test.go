package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

// catchAllSamples cover every mode a path-dependent route can resolve to.
var catchAllSamples = []string{"x", "x:generateContent", "x:predictLongRunning", "x:download"}

func probePaths(pattern string) []string {
	paths := []string{""}

	for _, segment := range strings.Split(strings.TrimPrefix(pattern, "/"), "/") {
		values := []string{segment}

		switch {
		case strings.HasPrefix(segment, ":"):
			values = []string{"x"}
		case strings.HasPrefix(segment, "*"):
			values = catchAllSamples
		}

		next := make([]string, 0, len(paths)*len(values))
		for _, prefix := range paths {
			for _, value := range values {
				next = append(next, prefix+"/"+value)
			}
		}

		paths = next
	}

	return paths
}

// Every non-GET/DELETE route is sent through its real handler chain with the
// admin key, so the distributor sets the route's mode. A route reached by POST
// whose mode is a read must be in the middleware's read-route table with that
// mode, and nothing else may be: a missing entry would count TokenAuth
// rejections on a read route as calls, a stale one would hide real calls.
func TestReadRouteModesMatchHandlers(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousAdminKey := config.AdminKey
	config.AdminKey = "read-route-probe-admin-key"

	t.Cleanup(func() { config.AdminKey = previousAdminKey })

	database, err := model.OpenSQLite(filepath.Join(t.TempDir(), "read-route-modes.db"))
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.Log{}, &model.RequestDetail{}))

	previousLogDB := model.LogDB
	model.LogDB = database

	t.Cleanup(func() {
		model.LogDB = previousLogDB

		if sqlDB, sqlErr := database.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	})

	type probe struct {
		mode     mode.Mode
		readMode mode.Mode
		listed   bool
		panicked any
	}

	var last probe

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		last = probe{}

		defer func() {
			if recovered := recover(); recovered != nil {
				last.panicked = recovered
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()

		c.Next()

		last.mode = middleware.GetMode(c)
		last.readMode, last.listed = middleware.ReadRouteMode(c)
	})
	SetRelayRouter(engine)

	probed, reads := 0, 0

	for _, route := range engine.Routes() {
		if route.Method == http.MethodGet || route.Method == http.MethodDelete {
			continue
		}

		for _, path := range probePaths(route.Path) {
			request := httptest.NewRequestWithContext(t.Context(), route.Method, path, strings.NewReader(`{}`))
			request.Header.Set("Authorization", "Bearer "+config.AdminKey)
			request.Header.Set("Content-Type", "application/json")

			engine.ServeHTTP(httptest.NewRecorder(), request)

			description := fmt.Sprintf("%s %s (%s)", route.Method, route.Path, path)
			require.Nil(t, last.panicked, description)

			probed++

			if model.IsReadMode(last.mode) {
				reads++

				require.True(t, last.listed, "%s resolves to read mode %s but is not in readRouteModes", description, last.mode)
				require.Equal(t, last.mode, last.readMode, description)

				continue
			}

			require.False(t, last.listed, "%s is listed as read mode %s but its handler sets %s", description, last.readMode, last.mode)
		}
	}

	require.Positive(t, probed)
	// alpha/search, responses cancel, and Gemini file downloads on v1 and v1beta.
	require.Equal(t, 4, reads)
}
