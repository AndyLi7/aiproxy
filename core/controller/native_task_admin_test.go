package controller

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestGroupNativeTaskReadIsScopedToTheGroupAndHidesPlatformParameters(t *testing.T) {
	database, err := model.OpenSQLite(filepath.Join(t.TempDir(), "tasks.db"))
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.NativeTask{}))
	sqlDB, err := database.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	contract := `{"version":1,"model":"vendor/model/text-to-image","input_schema":{},"output_schema":{},"fixed_parameters":{"enable_safety_checker":true}}`
	require.NoError(t, database.Create(&model.NativeTask{
		ID: "task-1", GroupID: "customer", TokenID: 7, Model: "vendor/model/text-to-image", Fingerprint: "f",
		OutputSchema: "{}", OutputSchemaHash: "h", Status: "failed", ErrorCode: "upstream_task_failed",
		FrozenContract: contract, NativeInput: `{"prompt":"a lion at dawn","image_size":"square_hd","enable_safety_checker":true}`,
		UpstreamID: "private-upstream", Endpoint: "fal-ai/private/endpoint", ChannelID: 9,
	}).Error)

	oldKey := config.AdminKey
	config.AdminKey = "native-history-admin"
	t.Cleanup(func() { config.AdminKey = oldKey })
	reader := groupNativeTaskReader{engine: func() (*nativetask.Engine, bool) {
		return &nativetask.Engine{DB: database}, true
	}}
	router := gin.New()
	api := router.Group("/api", middleware.AdminAuth)
	api.GET("/native-tasks/:group/:id", reader.detail)
	get := func(path, key string) *httptest.ResponseRecorder {
		request := httptest.NewRequest("GET", path, nil)
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}

	require.Equal(t, 401, get("/api/native-tasks/customer/task-1", "").Code)
	require.Equal(t, 404, get("/api/native-tasks/other-group/task-1", config.AdminKey).Code)

	response := get("/api/native-tasks/customer/task-1", config.AdminKey)
	require.Equal(t, 200, response.Code)
	var body struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, "failed", body.Data["status"])
	require.Equal(t, "upstream_task_failed", body.Data["errorCode"])
	require.NotContains(t, body.Data, "errorIssues")
	require.JSONEq(t, `{"prompt":"a lion at dawn","image_size":"square_hd"}`, body.Data["inputJSON"].(string))
	// Routing and provider identity never leave the gateway.
	require.NotContains(t, response.Body.String(), "private-upstream")
	require.NotContains(t, response.Body.String(), "fal-ai/private/endpoint")
}

// Owner decision 2026-10-09: customer history names the rejected fields of an
// invalid_parameters task, as field paths and rule codes only.
func TestGroupNativeTaskReadListsRejectedParameters(t *testing.T) {
	database, err := model.OpenSQLite(filepath.Join(t.TempDir(), "tasks.db"))
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.NativeTask{}))
	sqlDB, err := database.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, database.Create(&model.NativeTask{
		ID: "task-voice", GroupID: "customer", TokenID: 7, Model: "vendor/model/text-to-speech", Fingerprint: "f",
		OutputSchema: "{}", OutputSchemaHash: "h", Status: "failed", ErrorCode: "invalid_parameters",
		PublicError:    `{"issues":[{"field":"voice","rule":"unsupported_value"}]}`,
		FrozenContract: `{"version":1,"model":"vendor/model/text-to-speech","input_schema":{},"output_schema":{}}`, NativeInput: `{"voice":"NoSuchVoice123"}`,
		UpstreamID: "private-upstream",
	}).Error)
	reader := groupNativeTaskReader{engine: func() (*nativetask.Engine, bool) {
		return &nativetask.Engine{DB: database}, true
	}}
	router := gin.New()
	router.GET("/api/native-tasks/:group/:id", reader.detail)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "/api/native-tasks/customer/task-voice", nil))
	require.Equal(t, 200, response.Code)
	var body struct {
		Data struct {
			ErrorCode   string `json:"errorCode"`
			ErrorIssues []struct {
				Field string `json:"field"`
				Rule  string `json:"rule"`
			} `json:"errorIssues"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, "invalid_parameters", body.Data.ErrorCode)
	require.Len(t, body.Data.ErrorIssues, 1)
	require.Equal(t, "voice", body.Data.ErrorIssues[0].Field)
	require.Equal(t, "unsupported_value", body.Data.ErrorIssues[0].Rule)
}
