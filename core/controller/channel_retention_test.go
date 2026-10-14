//nolint:testpackage
package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func useChannelRetentionDB(t *testing.T) *gorm.DB {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "retention.db"))
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.ChannelTest{}, &model.ModelConfig{},
		&model.NativeTask{}, &model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))

	previousDB, previousLogDB, previousUsingSQLite := model.DB, model.LogDB, common.UsingSQLite
	model.DB, model.LogDB, common.UsingSQLite = db, db, true
	t.Cleanup(func() {
		model.DB, model.LogDB, common.UsingSQLite = previousDB, previousLogDB, previousUsingSQLite
		require.NoError(t, sqlDB.Close())
	})

	return db
}

type channelAdminResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

func serveChannelAdmin(t *testing.T, method string, id int, body string) (int, channelAdminResponse) {
	t.Helper()
	router := gin.New()
	router.PUT("/api/channel/:id", UpdateChannel)
	router.DELETE("/api/channel/:id", DeleteChannel)
	request := httptest.NewRequest(method, "/api/channel/"+strconv.Itoa(id), strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	var response channelAdminResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response), recorder.Body.String())

	return recorder.Code, response
}

func storedChannel(t *testing.T, id int) *model.Channel {
	t.Helper()
	channel, err := model.GetChannelByID(id)
	require.NoError(t, err)

	return channel
}

// PUT /api/channel/:id used to compare the patch with a copy that already
// held the new values, so no credential change was ever refused over HTTP.
func TestChannelPutRefusesCredentialChangeWhileTasksNeedIt(t *testing.T) {
	db := useChannelRetentionDB(t)

	ch := &model.Channel{Name: "fal", Type: model.ChannelTypeFal, Key: "old-key", BaseURL: "https://queue.fal.run"}
	require.NoError(t, db.Create(ch).Error)
	task := &model.NativeTask{ID: "native-in-flight", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "fp",
		OutputSchema: "{}", OutputSchemaHash: "sha", Status: "queued", ChannelID: ch.ID}
	require.NoError(t, db.Create(task).Error)

	for _, body := range []string{`{"key":"new-key"}`, `{"base_url":"https://example.invalid"}`, `{"proxy_url":"http://proxy.invalid:8080"}`} {
		status, response := serveChannelAdmin(t, http.MethodPut, ch.ID, body)
		require.NotEqual(t, http.StatusOK, status, body)
		require.False(t, response.Success)
		require.Contains(t, response.Message, "native tasks")
	}
	status, response := serveChannelAdmin(t, http.MethodDelete, ch.ID, "")
	require.NotEqual(t, http.StatusOK, status)
	require.Contains(t, response.Message, "native tasks")
	stored := storedChannel(t, ch.ID)
	require.Equal(t, "old-key", stored.Key)
	require.Equal(t, "https://queue.fal.run", stored.BaseURL)

	// A routing-only write and a write repeating the stored key stay allowed.
	status, response = serveChannelAdmin(t, http.MethodPut, ch.ID, `{"priority":3,"key":"old-key"}`)
	require.Equal(t, http.StatusOK, status, response.Message)
	require.EqualValues(t, 3, storedChannel(t, ch.ID).Priority)

	// Completed but unsettled still retains; completed and settled releases.
	require.NoError(t, db.Model(task).Updates(map[string]any{"status": "completed", "billing_settled": false}).Error)
	status, _ = serveChannelAdmin(t, http.MethodPut, ch.ID, `{"key":"new-key"}`)
	require.NotEqual(t, http.StatusOK, status)
	require.NoError(t, db.Model(task).Updates(map[string]any{"status": "completed", "billing_settled": true}).Error)
	status, response = serveChannelAdmin(t, http.MethodPut, ch.ID, `{"key":"new-key"}`)
	require.Equal(t, http.StatusOK, status, response.Message)
	require.Equal(t, "new-key", storedChannel(t, ch.ID).Key)
}

// The image guard had the same HTTP hole.
func TestChannelPutRefusesCredentialChangeWhileImageTasksNeedIt(t *testing.T) {
	db := useChannelRetentionDB(t)

	ch := &model.Channel{Name: "fal", Type: model.ChannelTypeFal, Key: "old-key"}
	require.NoError(t, db.Create(ch).Error)
	_, _, err := model.ReserveImageTask(
		&model.ImageTask{ID: "image-in-flight", GroupID: "g", TokenID: 1},
		&model.AsyncUsageInfo{ChannelID: ch.ID},
	)
	require.NoError(t, err)

	status, response := serveChannelAdmin(t, http.MethodPut, ch.ID, `{"key":"new-key"}`)
	require.NotEqual(t, http.StatusOK, status)
	require.Contains(t, response.Message, "image tasks")
	require.Equal(t, "old-key", storedChannel(t, ch.ID).Key)
}

// Without tasks naming the channel, the same write goes through.
func TestChannelPutAllowsCredentialChangeWithoutTasks(t *testing.T) {
	db := useChannelRetentionDB(t)

	ch := &model.Channel{Name: "fal", Type: model.ChannelTypeFal, Key: "old-key"}
	other := &model.Channel{Name: "other", Type: model.ChannelTypeFal, Key: "other-key"}
	require.NoError(t, db.Create(ch).Error)
	require.NoError(t, db.Create(other).Error)
	require.NoError(t, db.Create(&model.NativeTask{ID: "native-other", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "fp",
		OutputSchema: "{}", OutputSchemaHash: "sha", Status: "queued", ChannelID: other.ID}).Error)

	status, response := serveChannelAdmin(t, http.MethodPut, ch.ID, `{"key":"new-key"}`)
	require.Equal(t, http.StatusOK, status, response.Message)
	require.True(t, response.Success)
	require.Equal(t, "new-key", storedChannel(t, ch.ID).Key)
}
