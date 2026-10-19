//nolint:testpackage // White-box coverage needs the package-private nativeRuntimeFeatures fixture or implementation.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

type nativeControllerWallet struct{ balance.GroupBalance }

func (nativeControllerWallet) Prepayment(
	_ context.Context,
	c balance.PrepaymentCommand,
) (balance.PrepaymentReceipt, error) {
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
	router.ServeHTTP(
		response,
		httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/model-tasks", nil),
	)
	require.Equal(t, 503, response.Code)
	require.NoError(t, db.Exec("CREATE TABLE native_tasks (id TEXT PRIMARY KEY)").Error)

	_, ready = NativeTaskEngine()
	require.False(t, ready, "partial migration must not advertise native execution")
	require.NoError(
		t,
		db.Exec("DROP TABLE native_tasks").Error,
	) // isolated empty test database only
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

	require.NoError(
		t,
		db.Create(
			&model.NativeTask{
				ID:                 "task",
				GroupID:            "owner",
				TokenID:            7,
				Model:              "native",
				Fingerprint:        "fp",
				OutputSchema:       `{}`,
				OutputSchemaHash:   "sha",
				Status:             "completed",
				DeliveredOutput:    "null",
				NativeOutput:       `{"secret":true}`,
				BillingOperationID: "native:task",
				CredentialScope:    "private",
			},
		).Error,
	)

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
		router.ServeHTTP(
			response,
			httptest.NewRequestWithContext(
				t.Context(),
				http.MethodGet,
				"/v1/model-tasks/task",
				nil,
			),
		)
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
	require.NoError(
		t,
		db.Create(
			&model.NativeTask{
				ID:                 "task",
				GroupID:            "owner",
				TokenID:            7,
				Model:              "native",
				Fingerprint:        hex.EncodeToString(digest[:]),
				OutputSchema:       `{}`,
				OutputSchemaHash:   "sha",
				Status:             "completed",
				DeliveredOutput:    "null",
				BillingOperationID: "native:task",
			},
		).Error,
	)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(middleware.Group, model.GroupCache{ID: "owner"})
		c.Set(middleware.Token, model.TokenCache{ID: 7})
		c.Next()
	})
	router.POST("/v1/model-tasks", NativeTasks()...)

	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/model-tasks",
		strings.NewReader(body),
	)
	request.Header.Set("X-Request-ID", "task")

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
	oldMeterSwitch, oldPublicIDSwitch := config.DisableNativeInputMeter, config.DisablePublicAPIIDs
	t.Cleanup(func() {
		config.DisableNativeInputMeter, config.DisablePublicAPIIDs = oldMeterSwitch, oldPublicIDSwitch
	})

	config.DisableNativeInputMeter, config.DisablePublicAPIIDs = false, false

	t.Setenv("EXTERNAL_BALANCE_URL", "https://storage.example")
	t.Setenv("EXTERNAL_BALANCE_KEY", "isolated-test-key")
	require.Empty(t, nativeRuntimeFeatures())
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	require.ElementsMatch(
		t,
		[]string{
			"native_task_v1",
			"native_prepayment_recovery_v1",
			"native_private_trial_v1",
			"native_owned_artifact_v1",
			"actual_cost_prepayment_v1",
			"native_input_meter_v1",
			"public_api_id_v1",
		},
		nativeRuntimeFeatures(),
	)
	// DISABLE_PUBLIC_API_IDS=true: the application stops writing public IDs.
	config.DisablePublicAPIIDs = true

	require.NotContains(t, nativeRuntimeFeatures(), "public_api_id_v1")
	require.Contains(t, nativeRuntimeFeatures(), "native_task_v1")

	config.DisablePublicAPIIDs = false
	engine, _ := NativeTaskEngine()
	require.True(t, engine.InputMeter)
	// DISABLE_NATIVE_INPUT_METER=true: no advertisement and no metered holds.
	config.DisableNativeInputMeter = true

	require.NotContains(t, nativeRuntimeFeatures(), "native_input_meter_v1")

	engine, _ = NativeTaskEngine()
	require.False(t, engine.InputMeter)
	t.Setenv("EXTERNAL_BALANCE_KEY", "")
	require.NotContains(t, nativeRuntimeFeatures(), "native_owned_artifact_v1")

	balance.Default = nil

	require.Empty(t, nativeRuntimeFeatures())
}

type nativeErrorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Native errors written by the controller used to carry only a code, and none
// reached the request log with a code or stage.
func TestNativeControllerErrorsUseSharedEnvelopeAndLogCode(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "native.db"))
	require.NoError(t, err)

	oldDB, oldWallet := model.LogDB, balance.Default
	t.Cleanup(func() { model.LogDB = oldDB; balance.Default = oldWallet })

	model.LogDB, balance.Default = db, nativeControllerWallet{}

	serve := func(group, header string, config map[model.ModelConfigKey]any, handlers ...gin.HandlerFunc) (*httptest.ResponseRecorder, model.OperationalFields) {
		var fields model.OperationalFields

		router := gin.New()
		router.Use(func(c *gin.Context) {
			c.Set(middleware.Group, model.GroupCache{ID: group})
			c.Set(middleware.Token, model.TokenCache{ID: 7})
			c.Set(middleware.ModelConfig, model.ModelConfig{Model: "native", Config: config})
			c.Next()
			fields = middleware.OperationalFieldsFromContext(c)
		})
		router.POST("/v1/model-tasks", handlers...)

		request := httptest.NewRequestWithContext(t.Context(),
			http.MethodPost,
			"/v1/model-tasks",
			strings.NewReader(`{"model":"native","input":{}}`),
		)
		request.Header.Set("X-Request-ID", header)
		request.Header.Set(AIProxyChannelHeader, "1") // non-internal groups cannot pin a channel

		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)

		return response, fields
	}
	check := func(t *testing.T, response *httptest.ResponseRecorder, fields model.OperationalFields, status int, code, kind string, stage model.FailureStage) {
		t.Helper()
		require.Equal(t, status, response.Code, response.Body.String())

		var body nativeErrorResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
		require.Equal(t, code, body.Error.Code)
		require.Equal(t, kind, body.Error.Type)
		require.Equal(t, nativetask.ErrorMessage(code), body.Error.Message)
		require.NotEqual(t, code, body.Error.Message)
		require.Equal(t, code, fields.ErrorCode)
		require.Equal(t, stage, fields.FailureStage)
	}

	response, fields := serve("owner", "task-a", nil, nativeTaskRuntime)
	check(
		t,
		response,
		fields,
		503,
		"native_execution_unavailable",
		"api_error",
		model.FailureStageRouting,
	)

	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))

	response, fields = serve("owner", "bad id", nil, replayNativeTask)
	check(
		t,
		response,
		fields,
		400,
		"invalid_request_id",
		"invalid_request_error",
		model.FailureStageValidation,
	)
	response, fields = serve("", "task-a", nil, replayNativeTask)
	check(t, response, fields, 401, "authentication_required", "api_error", model.FailureStageAuth)

	// A model without a native route is the caller's choice; a broken route is ours.
	response, fields = serve("owner", "task-a", nil, submitNativeTask)
	check(
		t,
		response,
		fields,
		400,
		"native_model_unavailable",
		"invalid_request_error",
		model.FailureStageValidation,
	)

	binding := map[string]any{"version": 1, "channelId": 1}
	response, fields = serve(
		"owner",
		"task-b",
		map[model.ModelConfigKey]any{NativeResultConfigKey: binding},
		submitNativeTask,
	)
	check(
		t,
		response,
		fields,
		503,
		"model_route_unavailable",
		"api_error",
		model.FailureStageRouting,
	)
	require.Equal(
		t,
		"This model is temporarily unavailable. Retry later with the same X-Request-Id.",
		nativetask.ErrorMessage("model_route_unavailable"),
	)

	var reserved int64
	require.NoError(t, db.Model(&model.NativeTask{}).Count(&reserved).Error)
	require.Zero(t, reserved, "a route failure must not reserve the request ID")
}

// Own task queue spec 2026-10-15 §6.2: cancel is scoped like a status read;
// only a waiting task is cancelled.
func TestNativeCancelOwnershipAndStates(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "native.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}, &model.ImageTask{}))

	previousDB, previousWallet := model.LogDB, balance.Default
	t.Cleanup(func() { model.LogDB = previousDB; balance.Default = previousWallet })

	model.LogDB = db
	balance.Default = nativeControllerWallet{}

	for id, status := range map[string]string{"waiting": model.NativeStatusWaiting, "sent": "queued"} {
		require.NoError(t, db.Create(&model.NativeTask{
			ID:                 id,
			GroupID:            "owner",
			TokenID:            7,
			Model:              "native",
			Fingerprint:        "fp",
			OutputSchema:       `{}`,
			OutputSchemaHash:   "sha",
			Status:             status,
			BillingOperationID: "native:" + id,
			QueueScope:         "fal:default",
		}).Error)
	}

	cancel := func(group string, token int, id string) *httptest.ResponseRecorder {
		router := gin.New()
		router.Use(func(c *gin.Context) {
			c.Set(middleware.Group, model.GroupCache{ID: group})
			c.Set(middleware.Token, model.TokenCache{ID: token})
			c.Next()
		})
		router.POST("/v1/model-tasks/:id/cancel", CancelNativeTask)

		response := httptest.NewRecorder()
		router.ServeHTTP(
			response,
			httptest.NewRequestWithContext(
				t.Context(),
				http.MethodPost,
				"/v1/model-tasks/"+id+"/cancel",
				nil,
			),
		)

		return response
	}
	require.Equal(t, 401, cancel("", 0, "waiting").Code)
	require.Equal(t, 404, cancel("other", 7, "waiting").Code)
	require.Equal(t, 404, cancel("owner", 8, "waiting").Code)
	require.Equal(
		t,
		model.NativeStatusWaiting,
		func() string { task, _ := model.GetNativeTask(db, "waiting", "owner", 7); return task.Status }(),
	)

	response := cancel("owner", 7, "waiting")
	require.Equal(t, 200, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"code":"cancelled"`)
	response = cancel("owner", 7, "sent")
	require.Equal(t, 409, response.Code)
	require.Contains(t, response.Body.String(), `"code":"task_not_cancellable"`)
}

// §7.3: the queue status needs the admin key and reports each scope.
func TestNativeQueueStatusEndpoint(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "native.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}, &model.ImageTask{}))

	previousDB, previousWallet := model.LogDB, balance.Default
	t.Cleanup(func() {
		model.LogDB = previousDB
		balance.Default = previousWallet

		config.SetNativeQueueLimits(map[string]int{})
	})

	model.LogDB = db
	balance.Default = nativeControllerWallet{}

	config.SetNativeQueueLimits(map[string]int{"fal:default": 8})
	require.NoError(t, db.Create(&model.NativeTask{
		ID:                 "w",
		GroupID:            "owner",
		TokenID:            7,
		Model:              "native",
		Fingerprint:        "fp",
		OutputSchema:       `{}`,
		OutputSchemaHash:   "sha",
		Status:             model.NativeStatusWaiting,
		BillingOperationID: "native:w",
		QueueScope:         "fal:default",
	}).Error)

	oldKey := config.AdminKey
	config.AdminKey = "queue-admin"
	t.Cleanup(func() { config.AdminKey = oldKey })

	router := gin.New()
	router.Group("/api", middleware.AdminAuth).GET("/native-queue/status", GetNativeQueueStatus)

	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"/api/native-queue/status",
		nil,
	)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, 401, response.Code)
	request.Header.Set("Authorization", "Bearer queue-admin")

	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, 200, response.Code)

	var body struct {
		Data nativetask.QueueStatus `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.False(t, body.Data.Enabled)
	require.Equal(t, int64(300), body.Data.TimeoutSeconds)
	require.Len(t, body.Data.Scopes, 1)
	require.Equal(t, "fal:default", body.Data.Scopes[0].Scope)
	require.Equal(t, 8, *body.Data.Scopes[0].Limit)
	require.Equal(t, int64(1), body.Data.Scopes[0].Waiting)
	require.Zero(t, body.Data.Scopes[0].InFlight)
}
