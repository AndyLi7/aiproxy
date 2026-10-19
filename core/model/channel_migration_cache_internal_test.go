package model

import (
	"testing"

	"github.com/labring/aiproxy/core/common/config"
	"github.com/stretchr/testify/require"
)

func TestMigrationCacheExactEffectiveNumbersAndOverlay(t *testing.T) {
	for _, mode := range []string{"safe", "unsafe-number", "yaml", "disabled-config"} {
		t.Run(mode, func(t *testing.T) {
			db, _ := useCacheRebuildDB(t)

			raw := `{"n":9007199254740993123456789}`
			if mode != "unsafe-number" {
				raw = `{"n":1.25}`
			}

			require.NoError(t, db.Exec("UPDATE model_configs SET config=?", raw).Error)

			var stored []ModelConfig
			require.NoError(t, db.Find(&stored).Error)

			wanted, e := cacheJSONValue([]byte(raw))
			require.NoError(t, e)

			s := cacheSnapshot{
				modelConfigs: stored,
				migrationSource: &migrationCacheSource{
					configValues: map[string][2]any{rebuildTestModel: {wanted, nil}},
				},
			}

			c := &ModelCaches{
				ModelConfig:  newModelConfigCache(stored),
				ChannelsByID: map[int]*Channel{},
			}
			if mode == "yaml" {
				c.ModelConfig = &yamlModelConfigCache{
					dbCache:     c.ModelConfig,
					yamlConfigs: map[string]ModelConfig{},
				}
			}

			old := config.DisableModelConfig
			defer func() { config.DisableModelConfig = old }()

			config.DisableModelConfig = mode == "disabled-config"
			result := finishMigrationCacheSource(s, c)
			require.Equal(t, mode == "safe", result.effective)

			if mode != "safe" {
				require.Equal(t, "effective_cache_projection_unsupported", result.reason)
			}
		})
	}
}

func TestMigrationCacheUnrelatedLegacyStillRebuilds(t *testing.T) {
	_, _ = useCacheRebuildDB(t)
	require.Equal(t, "cache_source_legacy", LoadModelCaches().migrationSource.reason)
	require.False(t, LoadModelCaches().migrationSource.effective)
	before := LoadModelCaches().Version

	require.NoError(t, InitModelConfigAndChannelCache())
	require.Greater(t, LoadModelCaches().Version, before)
}

func TestMigrationCacheExponentExpansionBounded(t *testing.T) {
	for _, raw := range []string{`{"n":1e999999999}`, `{"n":1e-999999999}`, `{"n":1e999999999999999999999999}`} {
		_, err := cacheJSONValue([]byte(raw))
		require.Error(t, err)
	}

	a, err := cacheJSONValue([]byte(`{"n":125e-2}`))
	require.NoError(t, err)
	b, err := cacheJSONValue([]byte(`{"n":1.25}`))
	require.NoError(t, err)
	require.Equal(t, a, b)
}

func TestMigrationCacheAdmissionOnlyNeverActivatedStillRefreshes(t *testing.T) {
	db, ch := useCacheRebuildDB(t)
	require.NoError(
		t,
		db.AutoMigrate(
			&ChannelAdmissionControl{},
			&ChannelAdmissionProof{},
			&ChannelAdmissionFamily{},
			&ChannelAdmissionBarrier{},
			&ChannelAdmissionUse{},
		),
	)
	require.NoError(t, db.Create(&ChannelAdmissionControl{ID: 1, Version: 1}).Error)
	require.NoError(
		t,
		db.Model(&Channel{}).
			Where("id=?", ch.ID).
			UpdateColumn("name", "updated-existing-channel").
			Error,
	)
	require.NoError(t, InitModelConfigAndChannelCache())
	require.Equal(t, "updated-existing-channel", LoadModelCaches().ChannelsByID[ch.ID].Name)
	require.Equal(t, "cache_source_legacy", LoadModelCaches().migrationSource.reason)
	require.False(t, LoadModelCaches().migrationSource.effective)
}
