package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type nativeControllerWallet struct{ balance.GroupBalance }

func (nativeControllerWallet) Prepayment(_ context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	return balance.PrepaymentReceipt{ID: c.BillingOperationID, Status: "settled"}, nil
}
func TestNativeControllerRequiresSeparateStorageAndWallet(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "native.db"))
	require.NoError(t, err)
	previousDB, previousWallet := model.LogDB, balance.Default
	t.Cleanup(func() { model.LogDB = previousDB; balance.Default = previousWallet })
	model.LogDB = db
	balance.Default = nativeControllerWallet{}
	_, ready := NativeTaskEngine()
	require.False(t, ready)
	router := gin.New()
	router.POST("/v1/model-tasks", NativeTasks()...)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/model-tasks", nil))
	require.Equal(t, 503, response.Code)
	require.NoError(t, db.Exec("CREATE TABLE native_tasks (id TEXT PRIMARY KEY)").Error)
	_, ready = NativeTaskEngine()
	require.False(t, ready, "partial migration must not advertise native execution")
	require.NoError(t, db.Exec("DROP TABLE native_tasks").Error) // isolated empty test database only
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	_, ready = NativeTaskEngine()
	require.True(t, ready)
	require.False(t, middleware.CheckRelayMode(mode.NativeTasks, mode.ImagesGenerations))
	require.False(t, middleware.CheckRelayMode(mode.ImagesGenerations, mode.NativeTasks))
	require.True(t, middleware.CheckRelayMode(mode.NativeTasks, mode.NativeTasks))
}
func TestNativeControllerStatusOwnershipAndNull(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "native.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	previousDB, previousWallet := model.LogDB, balance.Default
	t.Cleanup(func() { model.LogDB = previousDB; balance.Default = previousWallet })
	model.LogDB = db
	balance.Default = nativeControllerWallet{}
	require.NoError(t, db.Create(&model.NativeTask{ID: "task", GroupID: "owner", TokenID: 7, Model: "native", Fingerprint: "fp", OutputSchema: `{}`, OutputSchemaHash: "sha", Status: "completed", DeliveredOutput: "null", NativeOutput: `{"secret":true}`, BillingOperationID: "native:task", CredentialScope: "private"}).Error)
	for _, tc := range []struct {
		group         string
		token, status int
	}{{"owner", 7, 200}, {"owner", 8, 404}, {"other", 7, 404}, {"", 0, 401}} {
		router := gin.New()
		router.Use(func(c *gin.Context) {
			c.Set(middleware.Group, model.GroupCache{ID: tc.group})
			c.Set(middleware.Token, model.TokenCache{ID: tc.token})
			c.Next()
		})
		router.GET("/v1/model-tasks/:id", GetNativeTask)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/model-tasks/task", nil))
		require.Equal(t, tc.status, response.Code)
		require.NotContains(t, response.Body.String(), "secret")
		require.NotContains(t, response.Body.String(), "private")
		if tc.status == 200 {
			require.Contains(t, response.Body.String(), `"output":null`)
		}
	}
}

func TestNativeAcceptedReplayDoesNotRequireCurrentModelConfig(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "native.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	oldDB, oldWallet := model.LogDB, balance.Default
	t.Cleanup(func() { model.LogDB = oldDB; balance.Default = oldWallet })
	model.LogDB = db
	balance.Default = nativeControllerWallet{}
	body := `{"model":"native","input":{}}`
	digest := sha256.Sum256([]byte(body))
	require.NoError(t, db.Create(&model.NativeTask{ID: "task", GroupID: "owner", TokenID: 7, Model: "native", Fingerprint: hex.EncodeToString(digest[:]), OutputSchema: `{}`, OutputSchemaHash: "sha", Status: "completed", DeliveredOutput: "null", BillingOperationID: "native:task"}).Error)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(middleware.Group, model.GroupCache{ID: "owner"})
		c.Set(middleware.Token, model.TokenCache{ID: 7})
		c.Next()
	})
	router.POST("/v1/model-tasks", NativeTasks()...)
	request := httptest.NewRequest(http.MethodPost, "/v1/model-tasks", strings.NewReader(body))
	request.Header.Set("X-Request-Id", "task")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, 202, response.Code)
	require.Contains(t, response.Body.String(), `"output":null`)
}

func TestNativeRuntimeFeatureNegotiation(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "features.db"))
	require.NoError(t, err)
	oldDB, oldWallet := model.LogDB, balance.Default
	t.Cleanup(func() { model.LogDB = oldDB; balance.Default = oldWallet })
	model.LogDB, balance.Default = db, nativeControllerWallet{}
	t.Setenv("EXTERNAL_BALANCE_URL", "https://storage.example")
	t.Setenv("EXTERNAL_BALANCE_KEY", "isolated-test-key")
	require.Empty(t, nativeRuntimeFeatures())
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	require.ElementsMatch(t, []string{"native_task_v1", "native_prepayment_recovery_v1", "native_private_trial_v1", "native_owned_artifact_v1", "actual_cost_prepayment_v1"}, nativeRuntimeFeatures())
	t.Setenv("EXTERNAL_BALANCE_KEY", "")
	require.NotContains(t, nativeRuntimeFeatures(), "native_owned_artifact_v1")
	balance.Default = nil
	require.Empty(t, nativeRuntimeFeatures())
}
