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
	previousDB := model.DB
	previousLogDB := model.LogDB
	database, err := model.OpenSQLite(filepath.Join(t.TempDir(), "video-task-handler.db"))
	require.NoError(t, err)
	model.DB = database
	model.LogDB = database
	t.Cleanup(func() {
		model.DB = previousDB
		model.LogDB = previousLogDB
	})
	require.NoError(t, database.AutoMigrate(&model.Channel{}, &model.AsyncUsageInfo{}))
	require.NoError(t, database.Create(&model.Channel{
		ID:   9,
		Name: "ark-production",
		Type: model.ChannelTypeDoubao,
	}).Error)
	require.NoError(t, database.Create(&model.AsyncUsageInfo{
		RequestID:       "req-safe",
		RequestAt:       time.Now(),
		Mode:            int(mode.Videos),
		Model:           "seedance-1-5-pro",
		ChannelID:       9,
		BaseURL:         "https://upstream-secret.invalid/?api_key=base-url-secret",
		GroupID:         "group-a",
		TokenID:         17,
		TokenName:       "customer-display-name",
		PricingCurrency: "USD",
		UpstreamID:      "video-public-safe",
		Status:          model.AsyncUsageStatusCompleted,
		Amount:          model.Amount{UsedAmount: 0.25},
		Error:           "Bearer raw-upstream-secret",
		ProcessingToken: "lease-secret",
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "group", Value: "group-a"}}
	ctx.Request = httptest.NewRequest(
		http.MethodGet,
		"/api/video_tasks/group-a?page=1&per_page=20",
		nil,
	)

	GetGroupVideoTasks(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	var envelope struct {
		Success bool                     `json:"success"`
		Data    model.GroupVideoTaskPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.True(t, envelope.Success)
	require.EqualValues(t, 1, envelope.Data.Total)
	require.Len(t, envelope.Data.Items, 1)
	require.Equal(t, "req-safe", envelope.Data.Items[0].RequestID)

	body := recorder.Body.String()
	for _, forbidden := range []string{
		"upstream-secret.invalid",
		"base-url-secret",
		"raw-upstream-secret",
		"lease-secret",
		"processing_token",
		"base_url",
		"metadata",
	} {
		require.False(t, strings.Contains(body, forbidden), body)
	}
}

func TestGetGroupVideoTasksRejectsInvalidBoundsWithSafeError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "group", Value: "group-a"}}
	ctx.Request = httptest.NewRequest(
		http.MethodGet,
		"/api/video_tasks/group-a?page=1&per_page=101",
		nil,
	)

	GetGroupVideoTasks(ctx)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "invalid pagination")
}

func TestGetGroupVideoTasksRejectsExcessivePageWithSafeError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "group", Value: "group-a"}}
	ctx.Request = httptest.NewRequest(
		http.MethodGet,
		"/api/video_tasks/group-a?page=10001&per_page=20",
		nil,
	)

	GetGroupVideoTasks(ctx)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "invalid pagination")
}
