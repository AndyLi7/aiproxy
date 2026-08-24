//nolint:testpackage
package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

func TestGetGroupVideoTasksReturnsOnlySafeProjection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "video-task-handler.db"))
	require.NoError(t, err)

	previousDB, previousLogDB := model.DB, model.LogDB
	model.DB, model.LogDB = db, db
	t.Cleanup(func() {
		model.DB, model.LogDB = previousDB, previousLogDB
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			require.NoError(t, sqlDB.Close())
		}
	})
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.AsyncUsageInfo{}))
	require.NoError(t, db.Create(&model.AsyncUsageInfo{
		RequestID:       "safe-request-id",
		Mode:            int(mode.VideoGenerationsJobs),
		Model:           "safe-model",
		ChannelID:       9,
		BaseURL:         "https://signed.example/secret-video-url",
		GroupID:         "group-a",
		TokenName:       "safe-token-name",
		PricingCurrency: "USD",
		UpstreamID:      "public-task-id",
		Status:          model.AsyncUsageStatusFailed,
		Amount:          model.Amount{UsedAmount: 0.25},
		Error:           "raw-error-prompt-secret",
		ProcessingToken: "processing-token-secret",
		CreatedAt:       time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 8, 24, 8, 1, 0, 0, time.UTC),
	}).Error)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "group", Value: "group-a"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/video_tasks/group-a?page=1&per_page=20", nil)

	GetGroupVideoTasks(c)
	require.Equal(t, http.StatusOK, w.Code)

	var response struct {
		Success bool                     `json:"success"`
		Data    model.GroupVideoTaskPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.True(t, response.Success)
	require.EqualValues(t, 1, response.Data.Total)
	require.Len(t, response.Data.Items, 1)
	require.Equal(t, "safe-request-id", response.Data.Items[0].RequestID)

	body := strings.ToLower(w.Body.String())
	for _, forbidden := range []string{
		"base_url",
		"processing_token",
		"raw-error-prompt-secret",
		"signed.example",
		"secret-video-url",
		`"price"`,
		`"error"`,
		`"prompt"`,
		`"key"`,
		`"metadata"`,
		`"video_url"`,
	} {
		require.NotContains(t, body, forbidden)
	}
}

func TestGetGroupVideoTasksRejectsInvalidBounds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, target := range []string{
		"/api/video_tasks/group-a?page=0&per_page=20",
		"/api/video_tasks/group-a?page=1&per_page=101",
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "group", Value: "group-a"}}
		c.Request = httptest.NewRequest(http.MethodGet, target, nil)

		GetGroupVideoTasks(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
		require.NotContains(t, w.Body.String(), "SQL")
	}
}
