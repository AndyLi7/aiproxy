//nolint:testpackage
package model

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Identity is every configs key except the application's routing keys; the
// comparison ignores key order and how a JSON library decoded the values.
func TestSameIdentityConfigs(t *testing.T) {
	t.Parallel()

	var stored ChannelConfigs
	require.NoError(t, sonic.Unmarshal([]byte(`{
		"region":"us","limits":{"a":1,"b":["x"]},"empty":null,
		"x_token_platform_provider_bindings":{"m":{"version":1}},"outbound_policy":"public_only"
	}`), &stored))

	for _, test := range []struct {
		name string
		next ChannelConfigs
		same bool
	}{
		{"identical", ChannelConfigs{"region": "us", "limits": map[string]any{"b": []any{"x"}, "a": float64(1)}}, true},
		{"routing keys only", ChannelConfigs{
			"region": "us", "limits": map[string]any{"a": int64(1), "b": []any{"x"}},
			"x_token_platform_provider_bindings": map[string]any{"n": 1}, "outbound_policy": "allow_private",
		}, true},
		{"null equals missing", ChannelConfigs{"region": "us", "limits": map[string]any{"a": 1, "b": []any{"x"}}, "other": nil}, true},
		{"value changed", ChannelConfigs{"region": "eu", "limits": map[string]any{"a": 1, "b": []any{"x"}}}, false},
		{"nested value changed", ChannelConfigs{"region": "us", "limits": map[string]any{"a": 2, "b": []any{"x"}}}, false},
		{"key removed", ChannelConfigs{"limits": map[string]any{"a": 1, "b": []any{"x"}}}, false},
		{"key added", ChannelConfigs{"region": "us", "limits": map[string]any{"a": 1, "b": []any{"x"}}, "project": "p"}, false},
		{"all cleared", ChannelConfigs{}, false},
		{"unencodable", ChannelConfigs{"region": "us", "limits": map[string]any{"a": 1, "b": []any{"x"}}, "bad": func() {}}, false},
	} {
		require.Equal(t, test.same, sameIdentityConfigs(test.next, stored), test.name)
		require.Equal(t, test.same, sameIdentityConfigs(stored, test.next), test.name+" (swapped)")
	}

	empty := ChannelConfigs{"outbound_policy": "public_only"}
	require.True(t, sameIdentityConfigs(empty, nil))
	require.True(
		t,
		sameIdentityConfigs(
			nil,
			ChannelConfigs{"x_token_platform_provider_bindings": map[string]any{}},
		),
	)
}

func useSnapshotTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "snapshot.db"))
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)

	previousDB, previousCaches, previousDefaults := DB, LoadModelCaches(), config.GetDefaultChannelModels()
	DB = db
	t.Cleanup(func() {
		DB = previousDB

		modelCaches.Store(previousCaches)
		config.SetDefaultChannelModels(previousDefaults)
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&Channel{}, &ModelConfig{}))

	return db
}

// A rebuild filters channel models against the model configs it read itself,
// not with a query per channel: the number of queries does not grow with the
// channels. The rules are unchanged: an empty list falls back to the type's
// default models, models without a config are dropped, the rest are sorted
// and kept once.
func TestCacheRebuildFiltersChannelModelsFromItsOwnRead(t *testing.T) {
	db := useSnapshotTestDB(t)

	config.SetDefaultChannelModels(map[int][]string{int(ChannelTypeOpenAI): {"vendor/default"}})

	for _, model := range []string{"vendor/a", "vendor/b"} {
		require.NoError(t, db.Create(&ModelConfig{Model: model, Type: mode.ChatCompletions}).Error)
	}

	mixed := &Channel{
		Name: "mixed", Type: ChannelTypeOpenAI, Key: "k",
		Models: []string{"vendor/b", "vendor/missing", "vendor/a", "vendor/a"},
	}
	fallback := &Channel{Name: "fallback", Type: ChannelTypeOpenAI, Key: "k"}
	orphan := &Channel{
		Name:   "orphan",
		Type:   ChannelTypeOpenAI,
		Key:    "k",
		Models: []string{"vendor/missing"},
	}
	retired := &Channel{
		Name: "retired", Type: ChannelTypeOpenAI, Key: "k", Status: ChannelStatusDisabled,
		Models: []string{"vendor/b", "vendor/missing"},
	}
	require.NoError(t, db.Create([]*Channel{mixed, fallback, orphan, retired}).Error)

	var queries atomic.Int64
	require.NoError(
		t,
		db.Callback().
			Query().
			After("gorm:query").
			Register("test:count_rebuild_queries", func(*gorm.DB) {
				queries.Add(1)
			}),
	)

	rebuild := func() int64 {
		t.Helper()
		queries.Store(0)
		require.NoError(t, InitModelConfigAndChannelCache())

		return queries.Load()
	}

	first := rebuild()
	caches := LoadModelCaches()
	require.Equal(t, []string{"vendor/a", "vendor/b"}, caches.ChannelsByID[mixed.ID].Models)
	require.Equal(t, []string{"vendor/default"}, caches.ChannelsByID[fallback.ID].Models)
	require.Empty(t, caches.ChannelsByID[orphan.ID].Models)
	require.Equal(t, []string{"vendor/b"}, caches.ChannelsByID[retired.ID].Models)
	require.Len(
		t,
		caches.EnabledModel2ChannelsBySet[ChannelDefaultSet]["vendor/a"],
		1,
		"a duplicate model routes once",
	)
	require.Contains(t, caches.DisabledModel2ChannelsBySet[ChannelDefaultSet], "vendor/b")

	for i := range 5 {
		require.NoError(t, db.Create(&Channel{
			Name: "more", Type: ChannelTypeOpenAI, Key: "k",
			Models: []string{"vendor/a", "vendor/missing"}, Priority: int32(i),
		}).Error)
	}

	require.Equal(t, first, rebuild(), "a rebuild must not query per channel")
	require.Len(t, LoadModelCaches().EnabledModel2ChannelsBySet[ChannelDefaultSet]["vendor/a"], 6)

	// With model configs disabled every channel model is kept, sorted.
	previousDisable := config.DisableModelConfig
	config.DisableModelConfig = true
	t.Cleanup(func() { config.DisableModelConfig = previousDisable })
	rebuild()
	require.Equal(t,
		[]string{"vendor/a", "vendor/a", "vendor/b", "vendor/missing"},
		LoadModelCaches().ChannelsByID[mixed.ID].Models,
	)
}

// On PostgreSQL one rebuild reads one snapshot. A write committed between its
// reads (here: right after the model configs are read, a channel is retired,
// its model config deleted, and a new channel and config published) used to
// leave a cache mixing both states; now the rebuild shows the state before the
// write, and the next rebuild the state after it.
func TestCacheRebuildPostgresReadsOneSnapshot(t *testing.T) {
	withTestPostgresStoreDB(t, func() {
		previousCaches := LoadModelCaches()
		t.Cleanup(func() { modelCaches.Store(previousCaches) })
		require.NoError(t, DB.AutoMigrate(&Channel{}, &ModelConfig{}))

		require.NoError(
			t,
			DB.Create(&ModelConfig{Model: "vendor/old", Type: mode.ChatCompletions}).Error,
		)
		old := &Channel{
			Name:   "old",
			Type:   ChannelTypeOpenAI,
			Key:    "k",
			Models: []string{"vendor/old"},
		}
		require.NoError(t, DB.Create(old).Error)

		var (
			armed   atomic.Bool
			written atomic.Bool
			late    = &Channel{
				Name:   "late",
				Type:   ChannelTypeOpenAI,
				Key:    "k",
				Models: []string{"vendor/late"},
			}
		)

		require.NoError(
			t,
			DB.Callback().
				Query().
				After("gorm:query").
				Register("test:write_between_rebuild_reads", func(tx *gorm.DB) {
					if tx.Statement.Table != "model_configs" || !armed.CompareAndSwap(true, false) {
						return
					}

					// Another connection commits while the rebuild is between reads.
					writer := DB.Session(&gorm.Session{NewDB: true})
					require.NoError(
						t,
						writer.Exec(
							"UPDATE channels SET status = ? WHERE id = ?",
							ChannelStatusDisabled,
							old.ID,
						).Error,
					)
					require.NoError(
						t,
						writer.Exec(
							"DELETE FROM model_configs WHERE model = ?",
							"vendor/old",
						).Error,
					)
					require.NoError(
						t,
						writer.Create(
							&ModelConfig{Model: "vendor/late", Type: mode.ChatCompletions},
						).Error,
					)
					require.NoError(t, writer.Create(late).Error)
					written.Store(true)
				}),
		)
		t.Cleanup(func() { _ = DB.Callback().Query().Remove("test:write_between_rebuild_reads") })

		armed.Store(true)
		require.NoError(t, InitModelConfigAndChannelCache())
		require.True(t, written.Load())

		before := LoadModelCaches()
		require.Equal(t, ChannelStatusEnabled, before.ChannelsByID[old.ID].Status)
		require.Equal(t, []string{"vendor/old"}, before.ChannelsByID[old.ID].Models)
		require.Contains(t, before.EnabledModelConfigsMap, "vendor/old")
		require.Len(t, before.EnabledModel2ChannelsBySet[ChannelDefaultSet]["vendor/old"], 1)
		require.NotContains(t, before.ChannelsByID, late.ID)
		require.NotContains(t, before.EnabledModelConfigsMap, "vendor/late")

		require.NoError(t, InitModelConfigAndChannelCache())

		after := LoadModelCaches()
		require.Equal(t, ChannelStatusDisabled, after.ChannelsByID[old.ID].Status)
		require.Empty(t, after.ChannelsByID[old.ID].Models)
		require.NotContains(t, after.EnabledModelConfigsMap, "vendor/old")
		require.Equal(t, []string{"vendor/late"}, after.ChannelsByID[late.ID].Models)
		require.Contains(t, after.EnabledModelConfigsMap, "vendor/late")
	})
}
