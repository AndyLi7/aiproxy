//nolint:testpackage // White-box coverage needs the package-private probeHTTPFixture fixture or implementation.
package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const cachePath = "/api/channel-migrations/operations/http-probe-op/cache"

func cacheRead(t *testing.T, f *probeHTTPFixture) model.MigrationCacheObservationResult {
	t.Helper()
	code, raw := f.call(t, "GET", cachePath, nil, config.AdminKey)
	require.Equal(t, 200, code)

	var v struct {
		Data model.MigrationCacheObservationResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &v))
	require.NotContains(t, string(raw), "synthetic-preparation-old")
	require.NotContains(t, string(raw), "synthetic-preparation-new")

	return v.Data
}

func TestMigrationCacheHTTPActualSwap(t *testing.T) {
	f := newProbeHTTPFixture(t)
	require.NoError(t, model.InitModelConfigAndChannelCache())
	require.Equal(t, "absent", cacheRead(t, f).State)
	retentionCommitted(t, f)
	r := cacheRead(t, f)
	require.Equal(t, "loaded", r.State, r.Reason)
	require.NotNil(t, r.Observation)
	require.Len(t, r.Observation.BootIncarnation, 64)
	require.Equal(t, "4", r.Observation.CommitRevision)
	old := r.Observation.CacheVersion

	require.NoError(t, model.InitModelConfigAndChannelCache())
	again := cacheRead(t, f)
	require.Equal(t, "loaded", again.State, again.Reason)
	require.NotEqual(t, old, again.Observation.CacheVersion)
	require.Equal(t, r.Observation.BootIncarnation, again.Observation.BootIncarnation)
	require.NoError(
		t,
		f.db.Model(&model.ChannelAdmissionFamily{}).
			Where("operation_id=?", "http-probe-op").
			UpdateColumn("operation_id", "superseded").
			Error,
	)
	require.Equal(t, "conflict", cacheRead(t, f).State)
}

func TestMigrationCacheHTTPBoundary(t *testing.T) {
	f := newProbeHTTPFixture(t)
	for _, entry := range []struct {
		path, key string
		body      []byte
		status    int
	}{
		{cachePath, "", nil, http.StatusUnauthorized},
		{cachePath, "wrong", nil, http.StatusUnauthorized},
		{cachePath + "?key=" + config.AdminKey, "", nil, http.StatusUnauthorized},
		{cachePath + "?extra=1", config.AdminKey, nil, http.StatusUnauthorized},
		{cachePath, config.AdminKey, []byte(`{}`), http.StatusBadRequest},
		{strings.Replace(cachePath, "http-probe-op", "_invalid", 1), config.AdminKey, nil, http.StatusBadRequest},
	} {
		code, _ := f.call(t, "GET", entry.path, entry.body, entry.key)
		require.Equal(t, entry.status, code)
	}
}

func TestMigrationCacheSQLiteNoDML(t *testing.T) {
	f := newProbeHTTPFixture(t)
	retentionCommitted(t, f)

	var tables []string
	require.NoError(
		t,
		f.db.Raw("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").
			Scan(&tables).
			Error,
	)

	for _, table := range tables {
		for _, kind := range []string{"INSERT", "UPDATE", "DELETE"} {
			require.NoError(
				t,
				f.db.Exec(
					"CREATE TRIGGER cache_no_"+strings.ToLower(
						kind,
					)+"_"+table+" BEFORE "+kind+" ON "+table+" BEGIN SELECT RAISE(ABORT, 'cache must not write'); END",
				).Error,
			)
		}
	}

	require.NoError(t, model.InitModelConfigAndChannelCache())
	require.Equal(t, "loaded", cacheRead(t, f).State)
	require.Error(t, f.db.Exec("UPDATE channel_admission_controls SET revision=revision").Error)
}

func TestMigrationCacheSwapNotPublishedEarly(t *testing.T) {
	f := newProbeHTTPFixture(t)
	b := prepareHTTPProbeForCutover(t, f)
	require.NoError(t, model.InitModelConfigAndChannelCache())

	reached, release := make(chan struct{}), make(chan struct{})

	var once atomic.Bool

	require.NoError(
		t,
		f.db.Callback().Query().After("gorm:query").Register("cache:pause", func(tx *gorm.DB) {
			// Called by rebuild after the DB cutover committed, before Store.
			if tx.Statement.Table == "channels" &&
				strings.Contains(tx.Statement.SQL.String(), "status =") &&
				once.CompareAndSwap(false, true) {
				close(reached)
				<-release
			}
		}),
	)
	defer func() {
		if err := f.db.Callback().Query().Remove("cache:pause"); err != nil {
			t.Error(err)
		}
	}()

	done := make(chan []byte, 1)
	go func() {
		_, raw := f.call(
			t,
			"POST",
			"/api/channel-migrations/operations/http-probe-op/cutover",
			b,
			config.AdminKey,
		)
		done <- raw
	}()

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("cache rebuild not reached")
	}

	r := cacheRead(t, f)
	require.Equal(t, "pending", r.State, r.Reason)
	require.Nil(t, r.Observation)
	close(release)
	require.Contains(t, string(<-done), `"state":"committed"`)
	require.Equal(t, "loaded", cacheRead(t, f).State)
}

func TestMigrationCacheUnsupportedPrecisionAndSchema(t *testing.T) {
	for _, mode := range []string{"partial", "default-models"} {
		t.Run(mode, func(t *testing.T) {
			f := newProbeHTTPFixture(t)
			retentionCommitted(t, f)

			if mode == "partial" {
				require.NoError(t, f.db.Migrator().DropTable(&model.ChannelAdmissionProof{}))

				_ = model.InitModelConfigAndChannelCache()

				require.NotEqual(t, "loaded", cacheRead(t, f).State)

				return
			}

			require.NoError(
				t,
				f.db.Model(&model.Channel{}).
					Where("id=?", f.body.Command.Seal.To.ChannelID).
					UpdateColumn("models", "[]").
					Error,
			)
			require.NoError(t, model.InitModelConfigAndChannelCache())
			require.NotEqual(t, "loaded", cacheRead(t, f).State)
		})
	}
}

func TestMigrationCacheFailedRebuildDoesNotAcknowledgeCommit(t *testing.T) {
	f := newProbeHTTPFixture(t)
	b := prepareHTTPProbeForCutover(t, f)
	require.NoError(t, model.InitModelConfigAndChannelCache())

	before := model.LoadModelCaches()

	require.NoError(
		t,
		f.db.Callback().Query().After("gorm:query").Register("cache:failure", func(tx *gorm.DB) {
			if tx.Statement.Table == "model_configs" &&
				strings.Contains(tx.Statement.SQL.String(), "created_at desc") {
				_ = tx.AddError(errors.New("synthetic cache build failure"))
			}
		}),
	)
	_, raw := f.call(
		t,
		"POST",
		"/api/channel-migrations/operations/http-probe-op/cutover",
		b,
		config.AdminKey,
	)
	require.Contains(t, string(raw), `"state":"committed"`)
	require.NoError(t, f.db.Callback().Query().Remove("cache:failure"))
	require.Same(t, before, model.LoadModelCaches())
	require.Equal(t, "pending", cacheRead(t, f).State)
	require.NoError(t, model.InitModelConfigAndChannelCache())
	require.Equal(t, "loaded", cacheRead(t, f).State)
}

func TestMigrationCacheEffectiveConfigurationDriftRejected(t *testing.T) {
	f := newProbeHTTPFixture(t)
	retentionCommitted(t, f)
	require.Equal(t, "loaded", cacheRead(t, f).State)

	cfg, ok := model.LoadModelCaches().ModelConfig.GetModelConfig("test/native::text-to-video")
	require.True(t, ok)
	// A mutable map accidentally changed by a consumer cannot retain loaded status.
	cfg.Config[model.ModelConfigKey("review_unknown")] = true
	r := cacheRead(t, f)
	require.Equal(t, "unsupported", r.State)
	require.Nil(t, r.Observation)
}

func TestMigrationCacheActualYAMLOverlayIsUnsupported(t *testing.T) {
	if os.Getenv("B2_CACHE_YAML_CHILD") != "1" {
		binary, e := os.Executable()
		require.NoError(t, e)

		cmd := exec.CommandContext(t.Context(),
			binary,
			"-test.run=^TestMigrationCacheActualYAMLOverlayIsUnsupported$",
			"-test.count=1",
		)

		cmd.Env = append(os.Environ(), "B2_CACHE_YAML_CHILD=1")
		out, e := cmd.CombinedOutput()
		require.NoError(t, e, string(out))

		return
	}

	f := newProbeHTTPFixture(t)
	retentionCommitted(t, f)
	require.Equal(t, "loaded", cacheRead(t, f).State)

	old := config.ConfigFilePath
	defer func() { config.ConfigFilePath = old }()

	path := filepath.Join(t.TempDir(), "cache-overlay.yaml")
	// An override with a colliding ID must not masquerade as the DB routing set.
	require.NoError(
		t,
		os.WriteFile(
			path,
			[]byte(
				"channels:\n  - id: 11\n    name: YAML override\n    type: 59\n    key: synthetic-overlay\n    status: 1\n    models: [test/native::text-to-video]\n",
			),
			0o600,
		),
	)
	config.ConfigFilePath = path

	require.NoError(t, model.InitModelConfigAndChannelCache())
	r := cacheRead(t, f)
	require.Equal(t, "unsupported", r.State)
	require.Nil(t, r.Observation)
}
