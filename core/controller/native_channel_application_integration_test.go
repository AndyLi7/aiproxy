package controller

import (
	"context"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNativePrivateChannelApplicationPreparation(t *testing.T) {
	appRoot := os.Getenv("NATIVE_TEST_APP_ROOT")
	if appRoot == "" {
		t.Skip("requires disposable application runner")
	}
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "channels.db"))
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	previousDB, previousLog := model.DB, model.LogDB
	previousSQLite, previousDisable, previousKey := common.UsingSQLite, config.DisableModelConfig, config.AdminKey
	model.DB, model.LogDB = db, db
	common.UsingSQLite = true
	config.DisableModelConfig = true
	config.AdminKey = "private-admin-test"
	t.Cleanup(func() {
		model.DB, model.LogDB = previousDB, previousLog
		common.UsingSQLite = previousSQLite
		config.DisableModelConfig = previousDisable
		config.AdminKey = previousKey
		_ = sqlDB.Close()
	})
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.ModelConfig{}, &model.SummaryMinute{}))
	for i := 0; i < 30; i++ {
		require.NoError(t, db.Create(&model.Channel{Name: fmt.Sprintf("unrelated-%d", i), Type: 59, Status: model.ChannelStatusDisabled}).Error)
	}
	router := gin.New()
	api := router.Group("/api", middleware.AdminAuth)
	api.POST("/channel/", AddChannel)
	api.GET("/channel/:id", GetChannel)
	api.GET("/channels", GetChannels)
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--import", "tsx", "tests/fixtures/native-channel-client.ts")
	cmd.Dir = appRoot
	cmd.Env = append(os.Environ(), "AIPROXY_BASE_URL="+server.URL, "AIPROXY_ADMIN_KEY=private-admin-test")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	t.Log(string(output))
	var channels []model.Channel
	require.NoError(t, db.Where("name LIKE ?", "native-private-%").Find(&channels).Error)
	require.Len(t, channels, 1)
	require.Equal(t, model.ChannelStatusDisabled, channels[0].Status)
	require.Empty(t, channels[0].Models)
	require.Empty(t, channels[0].ModelMapping)
	var count int64
	require.NoError(t, db.Model(&model.ModelConfig{}).Count(&count).Error)
	require.Zero(t, count)
}
