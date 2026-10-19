//nolint:testpackage // The private routingHTTPFixture fixture is shared with in-package tests.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

func routingHTTPFixture(t *testing.T) (*httptest.Server, *model.Channel) {
	t.Helper()
	db, e := model.OpenSQLite(filepath.Join(t.TempDir(), "routing.db"))
	require.NoError(t, e)
	pool, e := db.DB()
	require.NoError(t, e)

	oldDB, oldLog, oldKey := model.DB, model.LogDB, config.AdminKey
	model.DB, model.LogDB = db, db
	config.AdminKey = "synthetic-routing-admin"
	t.Cleanup(func() {
		model.DB, model.LogDB, config.AdminKey = oldDB, oldLog, oldKey

		require.NoError(t, pool.Close())
	})
	require.NoError(
		t,
		db.AutoMigrate(
			&model.Channel{},
			&model.ChannelTest{},
			&model.ModelConfig{},
			&model.ChannelRoutingState{},
			&model.ChannelRoutingReceipt{},
			&model.ChannelAccessConfiguration{},
		),
	)
	require.NoError(
		t,
		db.Create(&model.ModelConfig{Model: "routing-test", Type: mode.ChatCompletions}).Error,
	)
	ch := &model.Channel{
		Name:         "fal / fixture [shadow v3]",
		Type:         model.ChannelTypeOpenAI,
		Key:          "synthetic-routing-provider-key",
		BaseURL:      "https://example.invalid",
		Models:       []string{"routing-test"},
		ModelMapping: map[string]string{"routing-test": "upstream"},
		Sets:         []string{"synthetic-group"},
		Priority:     1,
	}
	require.NoError(t, db.Create(ch).Error)
	require.NoError(
		t,
		db.Create(&model.ChannelRoutingState{ChannelID: ch.ID, Version: 1, Revision: 1}).Error,
	)
	require.NoError(
		t,
		db.Model(ch).
			UpdateColumn("configs", `{"opaque":900719925474099312345,"x_token_platform_provider_bindings":{}}`).
			Error,
	)

	r := gin.New()
	api := r.Group("/api/channels", RequireMigrationInventoryHeaderAuth, middleware.AdminAuth)
	api.GET("/:id/routing", GetChannelRouting)

	access := r.Group(
		"/api/access-configurations",
		RequireMigrationInventoryHeaderAuth,
		middleware.AdminAuth,
	)
	access.GET("/:id", GetAccessConfiguration)
	access.PUT("/:id", EnsureAccessConfiguration)

	legacy := r.Group("/api", middleware.AdminAuth)
	legacy.GET("/channels/", GetChannels)
	legacy.GET("/channels/all", GetAllChannels)
	legacy.GET("/status", GetStatus)
	legacy.GET("/channel/:id", GetChannel)
	legacy.POST("/channel/", AddChannel)
	legacy.PUT("/channel/:id", UpdateChannel)
	legacy.POST("/channel/:id/status", UpdateChannelStatus)
	legacy.DELETE("/channel/:id", DeleteChannel)
	legacy.POST("/model_config/*model", SaveModelConfig)
	legacy.DELETE("/model_config/*model", DeleteModelConfig)
	legacy.GET("/model_config/*model", GetModelConfig)
	legacy.POST("/model_configs/", SaveModelConfigs)
	legacy.GET("/model_configs/all", GetAllModelConfigs)
	api.PATCH("/:id/routing", PatchChannelRouting)
	api.GET("/:id/routing/requests/:requestId", GetChannelRouting)

	server := httptest.NewServer(r)
	t.Cleanup(server.Close)

	return server, ch
}

func routingHTTPCall(
	t *testing.T,
	s *httptest.Server,
	method, path, key string,
	body []byte,
) (int, model.ChannelRoutingResult) {
	t.Helper()

	req, e := http.NewRequestWithContext(t.Context(), method, s.URL+path, bytes.NewReader(body))
	require.NoError(t, e)

	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	res, e := s.Client().Do(req)
	require.NoError(t, e)

	defer res.Body.Close()

	require.Equal(t, "no-store", res.Header.Get("Cache-Control"))
	raw, e := io.ReadAll(res.Body)
	require.NoError(t, e)

	var envelope struct {
		Data model.ChannelRoutingResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &envelope))
	require.NotContains(t, string(raw), "synthetic-routing-provider-key")

	return res.StatusCode, envelope.Data
}

func TestRoutingHTTPAuthAndStrictCAS(t *testing.T) {
	s, ch := routingHTTPFixture(t)
	path := "/api/channels/" + strconv.Itoa(ch.ID) + "/routing"
	code, r := routingHTTPCall(t, s, "GET", path, config.AdminKey, nil)
	require.Equal(t, 200, code)
	require.Equal(t, "ready", r.State)
	q := model.ChannelRoutingRequest{
		Version:             1,
		RequestID:           "http-write",
		ExpectedIdentity:    r.Snapshot.Identity,
		ExpectedRevision:    r.Snapshot.Revision,
		ExpectedRoutingHash: r.Snapshot.RoutingHash,
		NotAfterMs:          strconv.FormatInt(time.Now().Add(time.Minute).UnixMilli(), 10),
		Routing:             r.Snapshot.Routing,
	}
	q.Routing.Priority = 9
	rawContract, e := os.ReadFile("../common/registryvalidation/testdata/provider.json")
	require.NoError(t, e)

	binding := registryvalidation.ProviderBinding{
		Provider:     "small",
		ID:           "small",
		Revision:     "1",
		ContractHash: "sha256:" + strings.Repeat("a", 64),
	}
	_, e = registryvalidation.MapBoundProviderInput(
		rawContract,
		binding,
		"fal-image",
		"fal-ai/test",
		"async",
		[]byte(`{"model":"image","prompt":"hi","n":1}`),
	)
	require.NoError(t, e)

	q.Routing.ProviderBindings = map[string]registryvalidation.ProviderBinding{
		"routing-test": binding,
	}
	b, e := json.Marshal(q)
	require.NoError(t, e)

	for _, bad := range []struct {
		path, key string
		body      []byte
	}{{path, "", b}, {path + "?key=x", config.AdminKey, b}} {
		code, _ = routingHTTPCall(t, s, "PATCH", bad.path, bad.key, bad.body)
		require.Equal(t, 401, code)
	}

	for _, raw := range [][]byte{[]byte(strings.Replace(string(b), `"version":1`, `"version":1,"key":"forbidden"`, 1)), []byte(strings.Repeat(" ", model.ChannelRoutingBodyLimit+1))} {
		code, _ = routingHTTPCall(t, s, "PATCH", path, config.AdminKey, raw)
		require.Equal(t, 400, code)
	}

	code, r = routingHTTPCall(t, s, "PATCH", path, config.AdminKey, b)
	require.Equal(t, 200, code)
	require.Equal(t, "ready", r.State)
	require.Equal(t, "2", r.Receipt.After.Revision)
	_, read := routingHTTPCall(t, s, "GET", path+"/requests/http-write", config.AdminKey, nil)
	require.Equal(t, r.Receipt, read.Receipt)
	_, absent := routingHTTPCall(t, s, "GET", path+"/requests/not-seen", config.AdminKey, nil)
	require.Equal(t, "absent", absent.State)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Equal(t, "unavailable", model.ReadChannelRouting(ctx, ch.ID, "").State)
}

func TestRoutingHTTPInteropServer(t *testing.T) {
	dir := os.Getenv("B2_ROUTING_HTTP_FIXTURE_DIR")
	if dir == "" {
		t.Skip("explicit isolated HTTP interoperability fixture")
	}

	require.True(t, filepath.IsAbs(dir))
	// #nosec G703 -- The opt-in local integration harness owns this fixture or rendezvous path; no HTTP input supplies it.
	require.NoError(t, os.MkdirAll(dir, 0o700))
	s, ch := routingHTTPFixture(t)
	meta := map[string]any{
		"baseUrl":   s.URL,
		"adminKey":  config.AdminKey,
		"channelId": ch.ID,
		"channel":   ch,
		"snapshot":  model.ReadChannelRouting(context.Background(), ch.ID, "").Snapshot,
	}
	raw, e := json.Marshal(meta)
	require.NoError(t, e)

	path := filepath.Join(dir, "connection.json")

	// #nosec G703 -- The opt-in local integration harness owns this fixture or rendezvous path; no HTTP input supplies it.
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	defer func() {
		// #nosec G703 -- The opt-in local integration harness owns this fixture or rendezvous path; no HTTP input supplies it.
		if err := os.Remove(path); err != nil {
			t.Error(err)
		}
	}()

	timer := time.NewTimer(20 * time.Minute)
	defer timer.Stop()

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-timer.C:
			t.Fatal("fixture expired")
		case <-tick.C:
			// #nosec G703 -- The opt-in local integration harness owns this fixture or rendezvous path; no HTTP input supplies it.
			if _, e := os.Stat(filepath.Join(dir, "stop")); e == nil {
				return
			}
		}
	}
}
