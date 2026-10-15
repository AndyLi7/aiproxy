//nolint:testpackage
package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type channelRouteResponse struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// serveChannelRoute sends one admin request through the real channel handlers.
func serveChannelRoute(t *testing.T, method, path, body string) (int, channelRouteResponse, string) {
	t.Helper()
	router := gin.New()
	router.POST("/api/channel/", AddChannel)
	router.POST("/api/channels/", AddChannels)
	router.PUT("/api/channel/:id", UpdateChannel)
	router.POST("/api/channel/:id/status", UpdateChannelStatus)
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	var response channelRouteResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response), recorder.Body.String())

	return recorder.Code, response, recorder.Body.String()
}

func channelPath(id int, suffix string) string {
	return "/api/channel/" + strconv.Itoa(id) + suffix
}

func countChannels(t *testing.T, db *gorm.DB) int64 {
	t.Helper()

	var count int64
	require.NoError(t, db.Model(&model.Channel{}).Count(&count).Error)

	return count
}

// An explicit empty models list would make the channel serve every default
// model of its type; omitting models (or null) keeps the list as before.
func TestChannelPutRefusesExplicitEmptyModels(t *testing.T) {
	db := useChannelRetentionDB(t)
	require.NoError(t, db.Create(&model.ModelConfig{Model: "vendor/m", Type: mode.ChatCompletions}).Error)

	ch := &model.Channel{Name: "routes", Type: model.ChannelTypeOpenAI, Key: "k", Models: []string{"vendor/m"}}
	require.NoError(t, db.Create(ch).Error)

	status, response, _ := serveChannelRoute(t, http.MethodPut, channelPath(ch.ID, ""), `{"models":[]}`)
	require.Equal(t, http.StatusBadRequest, status)
	require.False(t, response.Success)
	require.Contains(t, response.Message, "models must not be an empty list")
	require.Equal(t, []string{"vendor/m"}, storedChannel(t, ch.ID).Models)

	for _, body := range []string{`{"models":null}`, `{"priority":4}`, `{"models":["vendor/m"]}`} {
		status, response, _ = serveChannelRoute(t, http.MethodPut, channelPath(ch.ID, ""), body)
		require.Equal(t, http.StatusOK, status, body+": "+response.Message)
		require.Equal(t, []string{"vendor/m"}, storedChannel(t, ch.ID).Models, body)
	}
}

// No admin write may newly produce an enabled channel with an empty key:
// creating one enabled, enabling one, or emptying an enabled channel's key.
func TestEnablingChannelWithEmptyKeyIsRefused(t *testing.T) {
	db := useChannelRetentionDB(t)

	// Status 0 takes the column default, enabled: that is why it is checked.
	status, response, _ := serveChannelRoute(t, http.MethodPost, "/api/channel/", `{"name":"keyed","type":1,"key":"k"}`)
	require.Equal(t, http.StatusOK, status, response.Message)

	var created ChannelCreateResponse
	require.NoError(t, json.Unmarshal(response.Data, &created))
	require.Equal(t, model.ChannelStatusEnabled, storedChannel(t, created.ID).Status)

	before := countChannels(t, db)

	for _, body := range []string{
		`{"name":"a","type":1,"key":""}`,
		`{"name":"a","type":1}`,
		`{"name":"a","type":1,"key":"","status":1}`,
		`{"name":"a","type":1,"key":"  \t","status":1}`,
	} {
		status, response, _ = serveChannelRoute(t, http.MethodPost, "/api/channel/", body)
		require.Equal(t, http.StatusBadRequest, status, body)
		require.Contains(t, response.Message, "channel key is empty", body)
	}

	// A batch with one such channel inserts nothing.
	status, response, _ = serveChannelRoute(t, http.MethodPost, "/api/channels/",
		`[{"name":"b","type":1,"key":"k","status":1},{"name":"c","type":1,"key":"","status":1}]`)
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, response.Message, "channel key is empty")
	require.Equal(t, before, countChannels(t, db))

	// Created disabled, it is allowed; enabling it is not, until it has a key.
	status, response, _ = serveChannelRoute(t, http.MethodPost, "/api/channel/", `{"name":"later","type":1,"key":"","status":2}`)
	require.Equal(t, http.StatusOK, status, response.Message)
	require.NoError(t, json.Unmarshal(response.Data, &created))
	id := created.ID

	status, response, _ = serveChannelRoute(t, http.MethodPost, channelPath(id, "/status"), `{"status":1}`)
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, response.Message, "channel key is empty")
	require.Equal(t, model.ChannelStatusDisabled, storedChannel(t, id).Status)

	status, _, _ = serveChannelRoute(t, http.MethodPost, channelPath(id, "/status"), `{"status":2}`)
	require.Equal(t, http.StatusOK, status)

	status, response, _ = serveChannelRoute(t, http.MethodPut, channelPath(id, ""), `{"key":"k2"}`)
	require.Equal(t, http.StatusOK, status, response.Message)
	status, response, _ = serveChannelRoute(t, http.MethodPost, channelPath(id, "/status"), `{"status":1}`)
	require.Equal(t, http.StatusOK, status, response.Message)
	require.Equal(t, model.ChannelStatusEnabled, storedChannel(t, id).Status)

	// Emptying the key of the enabled channel is refused; of a disabled one, not.
	for _, body := range []string{`{"key":""}`, `{"key":"   "}`} {
		status, response, _ = serveChannelRoute(t, http.MethodPut, channelPath(id, ""), body)
		require.Equal(t, http.StatusBadRequest, status, body)
		require.Contains(t, response.Message, "channel key is empty")
		require.Equal(t, "k2", storedChannel(t, id).Key)
	}

	status, _, _ = serveChannelRoute(t, http.MethodPost, channelPath(id, "/status"), `{"status":2}`)
	require.Equal(t, http.StatusOK, status)
	status, response, _ = serveChannelRoute(t, http.MethodPut, channelPath(id, ""), `{"key":""}`)
	require.Equal(t, http.StatusOK, status, response.Message)
	require.Empty(t, storedChannel(t, id).Key)

	// A row that is already enabled without a key (written before this rule)
	// is not a new one: re-enabling it and routine edits still work.
	legacy := &model.Channel{Name: "legacy", Type: model.ChannelTypeOpenAI, Status: model.ChannelStatusEnabled}
	require.NoError(t, db.Create(legacy).Error)
	status, response, _ = serveChannelRoute(t, http.MethodPost, channelPath(legacy.ID, "/status"), `{"status":1}`)
	require.Equal(t, http.StatusOK, status, response.Message)
	status, response, _ = serveChannelRoute(t, http.MethodPut, channelPath(legacy.ID, ""), `{"priority":5,"key":""}`)
	require.Equal(t, http.StatusOK, status, response.Message)
}

// skip_tls_verify and every configs key except the application's routing keys
// are credentials: while tasks still need the channel, changing them is
// refused like a key change. A binding or outbound_policy change is routing.
func TestChannelPutRetentionCoversTLSAndIdentityConfigs(t *testing.T) {
	db := useChannelRetentionDB(t)

	ch := &model.Channel{Name: "fal", Type: model.ChannelTypeFal, Key: "old-key", Configs: model.ChannelConfigs{
		"region":                             "us",
		"limits":                             map[string]any{"a": 1, "b": []any{"x"}},
		"x_token_platform_provider_bindings": map[string]any{"vendor/m": map[string]any{"version": 1}},
		"outbound_policy":                    "public_only",
	}}
	require.NoError(t, db.Create(ch).Error)
	task := &model.NativeTask{ID: "native-in-flight", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "fp",
		OutputSchema: "{}", OutputSchemaHash: "sha", Status: "queued", ChannelID: ch.ID}
	require.NoError(t, db.Create(task).Error)

	for _, body := range []string{
		`{"skip_tls_verify":true}`,
		// An identity value changed, removed, or added.
		`{"configs":{"region":"eu","limits":{"a":1,"b":["x"]},"outbound_policy":"public_only"}}`,
		`{"configs":{"limits":{"a":1,"b":["x"]},"outbound_policy":"public_only"}}`,
		`{"configs":{"region":"us","limits":{"a":1,"b":["x"]},"project":"p","outbound_policy":"public_only"}}`,
		`{"configs":{"region":"us","limits":{"a":2,"b":["x"]},"outbound_policy":"public_only"}}`,
		`{"configs":{}}`,
	} {
		status, response, _ := serveChannelRoute(t, http.MethodPut, channelPath(ch.ID, ""), body)
		require.NotEqual(t, http.StatusOK, status, body)
		require.False(t, response.Success, body)
		require.Contains(t, response.Message, "native tasks", body)
	}

	stored := storedChannel(t, ch.ID)
	require.False(t, stored.SkipTLSVerify)
	require.Equal(t, "us", stored.Configs["region"])

	// Routing-only writes go through: bindings and outbound_policy change,
	// identity values are repeated (key order and number form do not matter),
	// and skip_tls_verify repeats the stored value.
	for _, body := range []string{
		`{"configs":{"outbound_policy":"allow_private","limits":{"b":["x"],"a":1.0},"region":"us",` +
			`"x_token_platform_provider_bindings":{"vendor/n":{"version":1}}}}`,
		`{"configs":{"region":"us","limits":{"a":1,"b":["x"]},"outbound_policy":"public_only"}}`,
		`{"skip_tls_verify":false,"priority":7}`,
	} {
		status, response, _ := serveChannelRoute(t, http.MethodPut, channelPath(ch.ID, ""), body)
		require.Equal(t, http.StatusOK, status, body+": "+response.Message)
	}

	stored = storedChannel(t, ch.ID)
	require.Equal(t, "public_only", stored.Configs["outbound_policy"])
	require.NotContains(t, stored.Configs, "x_token_platform_provider_bindings")
	require.EqualValues(t, 7, stored.Priority)

	// Without tasks needing the channel, identity changes go through.
	require.NoError(t, db.Model(task).Updates(map[string]any{"status": "completed", "billing_settled": true}).Error)
	status, response, _ := serveChannelRoute(t, http.MethodPut, channelPath(ch.ID, ""),
		`{"skip_tls_verify":true,"configs":{"region":"eu"}}`)
	require.Equal(t, http.StatusOK, status, response.Message)
	stored = storedChannel(t, ch.ID)
	require.True(t, stored.SkipTLSVerify)
	require.Equal(t, "eu", stored.Configs["region"])
}

// The PUT response used to echo the stored key (S8). It keeps every other
// field of the channel JSON.
func TestChannelPutResponseOmitsKey(t *testing.T) {
	db := useChannelRetentionDB(t)
	require.NoError(t, db.Create(&model.ModelConfig{Model: "vendor/m", Type: mode.ChatCompletions}).Error)

	ch := &model.Channel{Name: "secret-holder", Type: model.ChannelTypeOpenAI, Key: "sk-stored-secret",
		BaseURL: "https://upstream.invalid", Models: []string{"vendor/m"},
		ModelMapping: map[string]string{"vendor/m": "m"}, Configs: model.ChannelConfigs{"region": "us"}}
	require.NoError(t, db.Create(ch).Error)

	for _, test := range []struct{ body, secret string }{
		{`{"priority":3}`, "sk-stored-secret"},
		{`{"key":"sk-replacement-secret"}`, "sk-replacement-secret"},
	} {
		status, response, raw := serveChannelRoute(t, http.MethodPut, channelPath(ch.ID, ""), test.body)
		require.Equal(t, http.StatusOK, status, response.Message)
		require.NotContains(t, raw, test.secret)
		require.NotContains(t, raw, "sk-stored-secret")

		var data map[string]any
		require.NoError(t, json.Unmarshal(response.Data, &data))
		require.NotContains(t, data, "key")

		for _, field := range []string{
			"id", "name", "type", "status", "base_url", "proxy_url", "models", "model_mapping", "configs",
			"priority", "backup_only", "skip_tls_verify", "created_at", "balance_updated_at", "last_test_error_at",
		} {
			require.Contains(t, data, field)
		}

		require.EqualValues(t, ch.ID, data["id"])
		require.Equal(t, "secret-holder", data["name"])
		require.Equal(t, []any{"vendor/m"}, data["models"])
		require.Equal(t, map[string]any{"region": "us"}, data["configs"])
		require.IsType(t, float64(0), data["created_at"], "timestamps stay in milliseconds")
	}

	require.Equal(t, "sk-replacement-secret", storedChannel(t, ch.ID).Key)
}

// native_route_follows_binding_v1 is advertised only while the switch is on
// and the native engine is configured; channel_routing_guard_v1 always.
func TestStatusAdvertisesChannelRoutingFeatures(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "status-native.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}, &model.NativeTask{}))

	previousLogDB, previousWallet, previousFollows := model.LogDB, balance.Default, config.NativeRouteFollowsBinding
	t.Cleanup(func() {
		model.LogDB, balance.Default, config.NativeRouteFollowsBinding = previousLogDB, previousWallet, previousFollows

		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	model.LogDB = db

	features := func() []string {
		t.Helper()

		recorder := getStatus(t)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

		var payload struct {
			Data StatusData `json:"data"`
		}
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))

		return payload.Data.Features
	}

	for _, test := range []struct {
		name       string
		configured bool
		follows    bool
		advertised bool
	}{
		{"configured and on", true, true, true},
		{"configured and switched off", true, false, false},
		{"not configured", false, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			balance.Default = nil
			if test.configured {
				balance.Default = nativeControllerWallet{}
			}

			config.NativeRouteFollowsBinding = test.follows

			got := features()
			require.Contains(t, got, ChannelRoutingGuardFeature)
			require.Equal(t, test.configured, slices.Contains(got, "native_task_v1"))
			require.Equal(t, test.advertised, slices.Contains(got, NativeRouteFollowsBindingFeature))
		})
	}
}
