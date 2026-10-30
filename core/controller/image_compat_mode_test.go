//nolint:testpackage // Exercises real configuration caches and image distributors.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/imagecompat"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func imageDisabledConfigFixture(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "disabled-config.db"))
	require.NoError(t, err)
	require.NoError(
		t,
		db.AutoMigrate(
			&model.Option{},
			&model.ModelConfig{},
			&model.Channel{},
			&model.NativeTask{},
		),
	)

	oldDB, oldLog, oldWallet := model.DB, model.LogDB, balance.Default
	oldCache, oldDisabled := *model.LoadModelCaches(), config.DisableModelConfig
	model.DB, model.LogDB, balance.Default = db, db, nativeControllerWallet{}
	config.DisableModelConfig = false
	*model.LoadModelCaches() = model.ModelCaches{}
	t.Cleanup(func() {
		model.DB, model.LogDB, balance.Default = oldDB, oldLog, oldWallet
		config.DisableModelConfig = oldDisabled
		*model.LoadModelCaches() = oldCache
		pool, e := db.DB()
		require.NoError(t, e)
		require.NoError(t, pool.Close())
	})
	t.Setenv("EXTERNAL_BALANCE_URL", "https://fixture.invalid")
	t.Setenv("EXTERNAL_BALANCE_KEY", strings.Repeat("s", 40))

	return db
}

func imageModeRequest(t *testing.T, requested, key string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/images/generations",
		strings.NewReader(`{"model":"`+requested+`","prompt":"mode fixture"}`),
	)
	req.Header.Set("Content-Type", "application/json")

	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}

	return req
}

func serveImageModeRequest(t *testing.T, req *http.Request) (*httptest.ResponseRecorder, int) {
	t.Helper()

	cache := model.LoadModelCaches()
	token := model.TokenCache{ID: 7}
	token.SetAvailableSets([]string{model.ChannelDefaultSet})
	token.SetModelsBySet(cache.EnabledModelsBySet)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(
			middleware.Group,
			model.GroupCache{ID: "disabled-config-owner", Status: model.GroupStatusInternal},
		)
		c.Set(middleware.Token, token)
		c.Set(middleware.ModelCaches, cache)
		c.Next()
	})

	handlers := imageCompatHandlers(mode.ImagesGenerations)
	calls := 0
	router.POST(
		"/v1/images/generations",
		handlers[0],
		handlers[1],
		func(c *gin.Context) { calls++; c.Status(http.StatusNoContent) },
	)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)

	return response, calls
}

func TestImageCompatDisabledModelConfigCannotClaimChannelOnlyLegacy(t *testing.T) {
	db := imageDisabledConfigFixture(t)
	config.DisableModelConfig = true
	legacy := "vendor/channel-only"
	candidate := imageProfileFixture(t, false)
	candidate.Config[model.ModelConfigPublicCapabilityAliasesKey] = []string{legacy}
	channel := model.Channel{
		Name:   "isolated-channel-only",
		Type:   model.ChannelTypeOpenAI,
		Key:    "synthetic-key",
		Models: []string{legacy, candidate.Model},
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, model.InitModelConfigAndChannelCache())

	_, exists := model.LoadModelCaches().EnabledModelConfigsMap[legacy]
	require.True(t, exists, "channel-only legacy must be present in the real cache")

	for _, key := range []string{"", "legacy-key"} {
		response, calls := serveImageModeRequest(t, imageModeRequest(t, legacy, key))
		require.Equal(t, 204, response.Code, response.Body.String())
		require.Equal(t, 1, calls)
	}
	// Nonfatal assertions preserve the original failing distributor evidence.
	if err := model.SaveModelConfig(candidate); err == nil {
		t.Error("disabled-model-config mode accepted a new compatibility activation")
	}

	if model.ReadOpenAIImageIdentities(db).Known(legacy) {
		t.Error("failed activation left a permanent claim on the channel-only legacy name")
	}

	for _, key := range []string{"", "legacy-key-after-save"} {
		response, calls := serveImageModeRequest(t, imageModeRequest(t, legacy, key))
		if response.Code != 204 || calls != 1 {
			t.Errorf(
				"channel-only legacy key=%q: status=%d calls=%d body=%s",
				key,
				response.Code,
				calls,
				response.Body.String(),
			)
		}
	}

	require.NotContains(t, nativeRuntimeFeatures(), "openai_images_native_v1")

	var count int64
	require.NoError(
		t,
		db.Model(&model.ModelConfig{}).Where("model=?", candidate.Model).Count(&count).Error,
	)
	require.Zero(t, count)
	require.Equal(t, model.ImageIdentityMissing, model.ReadOpenAIImageIdentities(db).State)
}

func TestImageCompatDisabledModelConfigPreservesOwnedRecovery(t *testing.T) {
	db := imageDisabledConfigFixture(t)
	require.NoError(t, model.InitModelConfigAndChannelCache())
	require.Contains(t, nativeRuntimeFeatures(), "openai_images_native_v1")
	cfg := imageProfileFixture(t, false)
	cfg.Config[model.ModelConfigPublicCapabilityAliasesKey] = []string{"historical/mode-image"}
	require.NoError(t, model.SaveModelConfig(cfg))

	legacy := "vendor/channel-only"
	channel := model.Channel{
		Name:   "isolated-mode-switch",
		Type:   model.ChannelTypeOpenAI,
		Key:    "synthetic-key",
		Models: []string{legacy, cfg.Model},
	}
	require.NoError(t, db.Create(&channel).Error)

	config.DisableModelConfig = true

	require.NoError(t, model.InitModelConfigAndChannelCache())
	require.NotContains(t, nativeRuntimeFeatures(), "openai_images_native_v1")
	require.True(t, model.LoadModelCaches().ImageIdentityEvidence().Known("historical/mode-image"))

	for _, key := range []string{"", "unrelated-key"} {
		response, calls := serveImageModeRequest(t, imageModeRequest(t, legacy, key))
		require.Equal(t, 204, response.Code, response.Body.String())
		require.Equal(t, 1, calls)
	}
	// An unknown accepted operation must stay closed even when native storage fails.
	model.LogDB = nil
	response, calls := serveImageModeRequest(
		t,
		imageModeRequest(t, "historical/mode-image", "accepted-key"),
	)
	require.Equal(t, 503, response.Code)
	require.Zero(t, calls)
	require.Contains(t, response.Body.String(), "task_store_unavailable")

	model.LogDB = db
	task, _ := signedImageFixture(t)
	req := imageModeRequest(t, imagecompat.GenerateModel, "accepted-key")
	id, err := imagecompat.OperationID(req.Header, "disabled-config-owner", 7)
	require.NoError(t, err)
	parsed, err := imagecompat.Parse(
		imageModeRequest(t, imagecompat.GenerateModel, "accepted-key"),
		false,
	)
	require.NoError(t, err)

	sum := sha256.Sum256(parsed.Body)
	task.ID, task.GroupID, task.Status, task.Fingerprint = id, "disabled-config-owner", "queued", hex.EncodeToString(
		sum[:],
	)
	require.NoError(t, db.Create(task).Error)

	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()

	require.NoError(
		t,
		db.Callback().
			Query().
			After("gorm:query").
			Register("cancel-after-owned-mode-lookup", func(tx *gorm.DB) {
				if tx.Statement.Table == "native_tasks" {
					cancel()
				}
			}),
	)
	response, calls = serveImageModeRequest(t, req.WithContext(ctx))
	require.Equal(t, 503, response.Code)
	require.Zero(t, calls)
	require.Contains(t, response.Body.String(), "image_operation_pending")
	require.Equal(t, id, response.Header().Get("X-Musespan-Task-Id"))
	require.NoError(t, db.Callback().Query().Remove("cancel-after-owned-mode-lookup"))
	// The unsupported mode serves recovery, not fresh compatibility submissions.
	response, calls = serveImageModeRequest(t, imageModeRequest(t, imagecompat.GenerateModel, ""))
	require.Equal(t, 503, response.Code)
	require.Zero(t, calls)
	require.Contains(t, response.Body.String(), "image_compatibility_unavailable")

	config.DisableModelConfig = false

	require.NoError(t, model.InitModelConfigAndChannelCache())
	require.Contains(t, nativeRuntimeFeatures(), "openai_images_native_v1")
}
