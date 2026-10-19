//nolint:testpackage // Verify the real HTTP handler passes request cancellation to storage.
package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestChannelStatusHTTPHonorsCancelledRequest(t *testing.T) {
	db := useChannelRetentionDB(t)
	ch := model.Channel{
		Name:   "cancelled-status",
		Key:    "synthetic-status-key",
		Type:   model.ChannelTypeFal,
		Status: model.ChannelStatusEnabled,
	}
	require.NoError(t, db.Create(&ch).Error)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, channelPath(ch.ID, "/status"), strings.NewReader(`{"status":2}`)).
		WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")

	router := gin.New()
	router.POST("/api/channel/:id/status", UpdateChannelStatus)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusOK, rec.Code)

	var after model.Channel
	require.NoError(t, db.First(&after, ch.ID).Error)
	require.Equal(t, model.ChannelStatusEnabled, after.Status)
}
