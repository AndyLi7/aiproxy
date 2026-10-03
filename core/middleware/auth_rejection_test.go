//nolint:testpackage
package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
	create := func(token model.Token) string {
		token.GroupID = "auth-group"
		require.NoError(t, db.Create(&token).Error)
		return token.Key
	}
	disabled := create(model.Token{Name: "secret-disabled-name", Status: model.TokenStatusDisabled})
	exhausted := create(model.Token{Name: "secret-quota-name", Status: model.TokenStatusEnabled, Quota: 1, UsedAmount: 2})
	fenced := create(model.Token{Name: "secret-subnet-name", Status: model.TokenStatusEnabled, Subnets: []string{"10.9.8.0/24"}})
	orphan := create(model.Token{Name: "secret-orphan-name", Status: model.TokenStatusEnabled})
	require.NoError(t, db.Model(&model.Token{}).Where("key = ?", orphan).Update("group_id", "auth-group-deleted").Error)

	for _, tc := range []struct {
		name, key, code, kind, message string
		status                         int
		before                         func(t *testing.T)
	}{
		{name: "missing", key: "", status: 401, code: "invalid_api_key", kind: "authentication_error", message: "The API key is missing or invalid."},
		{name: "unknown", key: "unknown-key-for-auth-test", status: 401, code: "invalid_api_key", kind: "authentication_error", message: "The API key is missing or invalid."},
		{name: "disabled", key: disabled, status: 403, code: "api_key_disabled", kind: "permission_error", message: "This API key is disabled. Enable it in the console or use another key."},
		{name: "quota exhausted", key: exhausted, status: 429, code: "api_key_quota_exhausted", kind: "insufficient_quota", message: "This API key has reached its spending limit. Raise the key's limit, wait for its limit period to reset, or use another key."},
		{name: "outside subnet", key: fenced, status: 403, code: "api_key_ip_not_allowed", kind: "permission_error", message: "This API key cannot be used from this network address. Check the key's allowed IP ranges."},
		{name: "group lookup failure", key: orphan, status: 503, code: "auth_unavailable", kind: "api_error", message: "API key verification is temporarily unavailable. Retry later with the same request."},
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
		})
	}
}
