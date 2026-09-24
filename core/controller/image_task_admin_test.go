package controller

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestGroupImageTaskRecoveryIsScopedAndHidesProviderURL(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "tasks.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))
	previous := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = previous })
	t.Setenv("PUBLIC_IMAGE_BASE_URL", "https://gateway.example.com")
	expires := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, db.Create(&model.ImageTask{
		ID: "task-one", GroupID: "owner", TokenID: 7, Model: "public",
		KeyFingerprint: strings.Repeat("a", 64), Status: "completed", ArchiveRequired: true, ResultExpiresAt: &expires,
		Data: []model.ImageOutput{{URL: "https://provider.example/private.png"}},
	}).Error)

	router := gin.New()
	router.GET("/api/image_tasks/:group/:id", GetGroupImageTask)
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/api/image_tasks/owner/task-one", 200},
		{"/api/image_tasks/other/task-one", 404},
		{"/api/image_tasks/owner/missing", 404},
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
		require.Equal(t, tc.status, response.Code)
		require.NotContains(t, response.Body.String(), "provider.example")
		if tc.status == 200 {
			require.Contains(t, response.Body.String(), "gateway.example.com")
			require.NotContains(t, response.Body.String(), "key_fingerprint")
			require.Contains(t, response.Body.String(), expires.Format(time.RFC3339))
			require.Contains(t, response.Body.String(), `"result_availability":"stored"`)
		}
	}
}
