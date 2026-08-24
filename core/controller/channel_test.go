//nolint:testpackage
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestAddChannelReturnsSanitizedCreatedChannelID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousDB := model.DB
	previousLogDB := model.LogDB
	previousUsingSQLite := common.UsingSQLite
	previousDisableModelConfig := config.DisableModelConfig
	testDB, err := model.OpenSQLite(filepath.Join(t.TempDir(), "channel.db"))
	require.NoError(t, err)
	model.DB = testDB
	model.LogDB = nil
	common.UsingSQLite = true
	config.DisableModelConfig = true
	t.Cleanup(func() {
		model.DB = previousDB
		model.LogDB = previousLogDB
		common.UsingSQLite = previousUsingSQLite
		config.DisableModelConfig = previousDisableModelConfig
	})
	require.NoError(t, testDB.AutoMigrate(&model.Channel{}, &model.ModelConfig{}))

	body := []byte(`{"name":"created-channel","key":"test-key","base_url":"https://example.invalid","proxy_url":"https://proxy.invalid","type":1,"status":1}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	AddChannel(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	require.Contains(t, response.Data, "id")
	require.Positive(t, response.Data["id"])
	require.Equal(t, "created-channel", response.Data["name"])
	require.NotContains(t, response.Data, "key")
	require.NotContains(t, response.Data, "base_url")
	require.NotContains(t, response.Data, "proxy_url")
}

func TestAddChannelsReturnsSanitizedCreatedChannelIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousDB := model.DB
	previousLogDB := model.LogDB
	previousUsingSQLite := common.UsingSQLite
	previousDisableModelConfig := config.DisableModelConfig
	testDB, err := model.OpenSQLite(filepath.Join(t.TempDir(), "channels.db"))
	require.NoError(t, err)
	model.DB = testDB
	model.LogDB = nil
	common.UsingSQLite = true
	config.DisableModelConfig = true
	t.Cleanup(func() {
		model.DB = previousDB
		model.LogDB = previousLogDB
		common.UsingSQLite = previousUsingSQLite
		config.DisableModelConfig = previousDisableModelConfig
	})
	require.NoError(t, testDB.AutoMigrate(&model.Channel{}, &model.ModelConfig{}))

	body := []byte(`[
		{"name":"created-a","key":"secret-a","type":1,"status":1},
		{"name":"created-b","key":"secret-b","type":1,"status":1}
	]`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channels/", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	AddChannels(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool             `json:"success"`
		Data    []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	require.Len(t, response.Data, 2)
	for _, channel := range response.Data {
		require.Contains(t, channel, "id")
		require.Positive(t, channel["id"])
		require.NotContains(t, channel, "key")
	}
}

func TestAddChannelRequestToChannelPreservesNewlinesInKey(t *testing.T) {
	const key = "first-key\nsecond-key"

	channel, err := (&AddChannelRequest{
		Type: model.ChannelTypeOpenAI,
		Name: "channel",
		Key:  key,
	}).ToChannel()

	require.NoError(t, err)
	require.Equal(t, key, channel.Key)
}

func TestRunAutoTestBannedModelsHonorsConcurrencyLimit(t *testing.T) {
	const (
		concurrency = 7
		totalJobs   = 41
	)

	channels := map[string][]int64{
		"model-a": make([]int64, totalJobs),
	}
	for i := range totalJobs {
		channels["model-a"][i] = int64(i + 1)
	}

	var (
		currentActive atomic.Int32
		maxActive     atomic.Int32
		processed     atomic.Int32
	)

	deps := autoTestBannedModelsDeps{
		tryTestChannel: func(channelID int, modelName string) bool {
			return true
		},
		loadChannelByID: func(id int) (*model.Channel, error) {
			return &model.Channel{
				ID:     id,
				Name:   "channel",
				Type:   model.ChannelTypeOpenAI,
				Status: model.ChannelStatusEnabled,
				Models: []string{"model-a"},
			}, nil
		},
		testSingleModel: func(mc *model.ModelCaches, channel *model.Channel, modelName string, saveToDB bool) (*model.ChannelTest, error) {
			active := currentActive.Add(1)
			for {
				previous := maxActive.Load()
				if active <= previous || maxActive.CompareAndSwap(previous, active) {
					break
				}
			}

			time.Sleep(15 * time.Millisecond)

			processed.Add(1)
			currentActive.Add(-1)

			return &model.ChannelTest{Success: true}, nil
		},
		clearChannelModelErrors: func(ctx context.Context, modelName string, channelID int) error {
			return nil
		},
		notifyInfo:  func(title, message string) {},
		notifyError: func(title, message string) {},
	}

	runAutoTestBannedModels(log.NewEntry(log.StandardLogger()), channels, nil, concurrency, deps)

	require.Equal(t, int32(totalJobs), processed.Load())
	require.LessOrEqual(t, maxActive.Load(), int32(concurrency))
}

func TestRunAutoTestBannedModelsClearsWhenModelRemovedFromChannel(t *testing.T) {
	var (
		cleared        atomic.Int32
		testInvoked    atomic.Bool
		clearedChannel atomic.Int64
	)

	deps := autoTestBannedModelsDeps{
		tryTestChannel: func(channelID int, modelName string) bool {
			return true
		},
		loadChannelByID: func(id int) (*model.Channel, error) {
			return &model.Channel{
				ID:     id,
				Name:   "channel",
				Type:   model.ChannelTypeOpenAI,
				Status: model.ChannelStatusEnabled,
				Models: []string{"another-model"},
			}, nil
		},
		testSingleModel: func(mc *model.ModelCaches, channel *model.Channel, modelName string, saveToDB bool) (*model.ChannelTest, error) {
			testInvoked.Store(true)
			return &model.ChannelTest{Success: true}, nil
		},
		clearChannelModelErrors: func(ctx context.Context, modelName string, channelID int) error {
			cleared.Add(1)
			clearedChannel.Store(int64(channelID))
			return nil
		},
		notifyInfo:  func(title, message string) {},
		notifyError: func(title, message string) {},
	}

	runAutoTestBannedModels(
		log.NewEntry(log.StandardLogger()),
		map[string][]int64{"removed-model": {123}},
		nil,
		1,
		deps,
	)

	require.False(t, testInvoked.Load())
	require.Equal(t, int32(1), cleared.Load())
	require.Equal(t, int64(123), clearedChannel.Load())
}
