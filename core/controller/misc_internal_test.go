package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestStatusAdvertisesAdminDemoV1(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.GET("/api/status", GetStatus)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(
		recorder,
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/status", nil),
	)
	require.Equal(t, http.StatusOK, recorder.Code)

	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			StartTime int64    `json:"startTime"`
			Features  []string `json:"features"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.True(t, payload.Success)
	require.NotZero(t, payload.Data.StartTime)
	require.Equal(
		t,
		[]string{
			"token_platform.admin_demo.v1",
			"image_billing_v1",
			"image_billing_input_basis_v1",
			"image_task_media_v1",
			"image_task_batch_v1",
			"registry_provider_fal_async_v1",
			"registry_provider_fal_queue_v2",
			"registry_provider_ark_sync_task_v1",
		},
		payload.Data.Features,
	)
}
