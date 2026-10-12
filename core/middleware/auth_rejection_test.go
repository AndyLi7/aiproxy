//nolint:testpackage
package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// Every key problem used to be reported as invalid_api_key, so customers with a
// disabled key, an exhausted key limit or a database outage rotated good keys.
func TestTokenAuthReportsEachKeyProblemDistinctly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	oldDB, oldRedis := model.DB, common.RedisEnabled
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "auth.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Group{}, &model.Token{}))
	model.DB, common.RedisEnabled = db, false
	t.Cleanup(func() { model.DB, common.RedisEnabled = oldDB, oldRedis })

	require.NoError(t, db.Create(&model.Group{ID: "auth-group", Status: model.GroupStatusEnabled}).Error)
	create := func(token model.Token) model.Token {
		token.GroupID = "auth-group"
		require.NoError(t, db.Create(&token).Error)
		return token
	}
	disabled := create(model.Token{Name: "secret-disabled-name", Status: model.TokenStatusDisabled})
	exhausted := create(model.Token{Name: "secret-quota-name", Status: model.TokenStatusEnabled, Quota: 1, UsedAmount: 2})
	fenced := create(model.Token{Name: "secret-subnet-name", Status: model.TokenStatusEnabled, Subnets: []string{"10.9.8.0/24"}})
	orphan := create(model.Token{Name: "secret-orphan-name", Status: model.TokenStatusEnabled})
	require.NoError(t, db.Model(&model.Token{}).Where("key = ?", orphan.Key).Update("group_id", "auth-group-deleted").Error)

	// A key the gateway identified owns its rejection row (it counts as that
	// key's rejected call); an unknown key cannot be attributed.
	for _, tc := range []struct {
		name, key, code, kind, message string
		status                         int
		tokenID                        int
		groupID                        string
		before                         func(t *testing.T)
	}{
		{name: "missing", key: "", status: 401, code: "invalid_api_key", kind: "authentication_error", message: "The API key is missing or invalid."},
		{name: "unknown", key: "unknown-key-for-auth-test", status: 401, code: "invalid_api_key", kind: "authentication_error", message: "The API key is missing or invalid."},
		{name: "disabled", key: disabled.Key, status: 403, code: "api_key_disabled", kind: "permission_error", message: "This API key is disabled. Enable it in the console or use another key.", tokenID: disabled.ID, groupID: "auth-group"},
		{name: "quota exhausted", key: exhausted.Key, status: 429, code: "api_key_quota_exhausted", kind: "insufficient_quota", message: "This API key has reached its spending limit. Raise the key's limit, wait for its limit period to reset, or use another key.", tokenID: exhausted.ID, groupID: "auth-group"},
		{name: "outside subnet", key: fenced.Key, status: 403, code: "api_key_ip_not_allowed", kind: "permission_error", message: "This API key cannot be used from this network address. Check the key's allowed IP ranges.", tokenID: fenced.ID, groupID: "auth-group"},
		{name: "group lookup failure", key: orphan.Key, status: 503, code: "auth_unavailable", kind: "api_error", message: "API key verification is temporarily unavailable. Retry later with the same request.", tokenID: orphan.ID, groupID: "auth-group-deleted"},
		{name: "database failure", key: "key-checked-while-database-is-down", status: 503, code: "auth_unavailable", kind: "api_error", message: "API key verification is temporarily unavailable. Retry later with the same request.", before: func(t *testing.T) {
			broken, err := model.OpenSQLite(filepath.Join(t.TempDir(), "broken.db"))
			require.NoError(t, err)
			sqlDB, err := broken.DB()
			require.NoError(t, err)
			require.NoError(t, sqlDB.Close())
			model.DB = broken
			t.Cleanup(func() { model.DB = db })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.before != nil {
				tc.before(t)
			}
			logs := captureOperationalLogs(t)
			router := gin.New()
			router.Use(OperationalLogMiddleware())
			router.POST("/v1/chat/completions", TokenAuth, func(c *gin.Context) { c.Status(http.StatusOK) })

			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
			request.RemoteAddr = "203.0.113.7:4000"
			if tc.key != "" {
				request.Header.Set("Authorization", "Bearer "+tc.key)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, request)

			require.Equal(t, tc.status, w.Code, w.Body.String())
			var body publicErrorBody
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			require.Equal(t, tc.code, body.Error.Code)
			require.Equal(t, tc.kind, body.Error.Type)
			require.Equal(t, tc.message, body.Error.Message)
			for _, private := range []string{"secret-", "10.9.8", "203.0.113", "auth-group"} {
				require.NotContains(t, w.Body.String(), private)
			}

			require.Len(t, *logs, 1)
			require.Equal(t, tc.status, (*logs)[0].Code)
			require.Equal(t, model.FailureStageAuth, (*logs)[0].FailureStage)
			require.Equal(t, tc.code, (*logs)[0].ErrorCode)
			require.NotContains(t, (*logs)[0].SafeError, "secret-")
			require.Equal(t, tc.tokenID, (*logs)[0].TokenID)
			require.Equal(t, tc.groupID, (*logs)[0].GroupID)
			require.Equal(t, tc.tokenID != 0, (*logs)[0].IsCall())
			require.Equal(t, model.CallStatusRejected, (*logs)[0].CallStatus(time.Now()))
		})
	}
}

// A disabled group is a key-check rejection like the others: stage auth,
// attributed to the key, with the response and error code unchanged.
func TestTokenAuthAttributesDisabledGroupRejection(t *testing.T) {
	gin.SetMode(gin.TestMode)

	oldRedis := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = oldRedis })

	token := model.TokenCache{ID: 41, Key: "paused-group-key", Name: "paused-key", Group: "paused-group", Status: model.TokenStatusEnabled}
	require.NoError(t, model.CacheSetToken(&token))
	require.NoError(t, model.CacheSetGroup(&model.GroupCache{ID: "paused-group", Status: model.GroupStatusDisabled}))
	t.Cleanup(func() {
		require.NoError(t, model.CacheDeleteToken(token.Key))
		require.NoError(t, model.CacheDeleteGroup("paused-group"))
	})

	logs := captureOperationalLogs(t)
	router := gin.New()
	router.Use(OperationalLogMiddleware())
	router.POST("/v1/chat/completions", TokenAuth, func(c *gin.Context) { c.Status(http.StatusOK) })

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer "+token.Key)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, request)

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var body publicErrorBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, "group is disabled", body.Error.Message)

	require.Len(t, *logs, 1)
	require.Equal(t, http.StatusForbidden, (*logs)[0].Code)
	require.Equal(t, model.FailureStageAuth, (*logs)[0].FailureStage)
	require.Equal(t, "access_denied", (*logs)[0].ErrorCode)
	require.Equal(t, token.ID, (*logs)[0].TokenID)
	require.Equal(t, "paused-key", (*logs)[0].TokenName)
	require.Equal(t, "paused-group", (*logs)[0].GroupID)
	require.True(t, (*logs)[0].IsCall())
	require.Equal(t, model.CallStatusRejected, (*logs)[0].CallStatus(time.Now()))
}
