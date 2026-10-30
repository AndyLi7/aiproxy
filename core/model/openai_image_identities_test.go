//nolint:testpackage // Exercises transactional configuration writes and immutable cache publication.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/labring/aiproxy/core/common/imagecompat"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const reviewedEmptyImageIdentities = `{"version":1,"capabilities":{}}`

func useImageIdentityDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "identities.db"))
	require.NoError(t, err)
	useImageIdentityDBOn(t, db)
	t.Cleanup(
		func() { sqlDB, e := db.DB(); require.NoError(t, e); require.NoError(t, sqlDB.Close()) },
	)

	return db
}

func useImageIdentityDBOn(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.AutoMigrate(&Option{}, &ModelConfig{}, &Channel{}))

	oldDB, oldLog, oldCache := DB, LogDB, LoadModelCaches()
	DB, LogDB = db, db

	modelCaches.Store(&ModelCaches{})
	t.Cleanup(func() { DB, LogDB = oldDB, oldLog; modelCaches.Store(oldCache) })
}

func imageIdentityConfig(alias string) ModelConfig {
	contract := json.RawMessage(
		`{"artifacts":[{"path":["images","*","url"]}],"input_schema":{"type":"object"},"model":"openai/gpt-image-1.5/text-to-image","output_schema":{"type":"object"},"version":1}`,
	)
	sum := sha256.Sum256(contract)

	return ModelConfig{
		Model: "openai/gpt-image-1.5::text-to-image", Type: mode.NativeTasks,
		Config: map[ModelConfigKey]any{
			ModelConfigCapabilityContractVersionKey: 1,
			ModelConfigPublicModelKey:               "openai/gpt-image-1.5",
			ModelConfigCapabilityKey:                "text-to-image",
			ModelConfigPublicCapabilityModelKey:     imagecompat.GenerateModel,
			ModelConfigPublicCapabilityAliasesKey:   []string{alias},
			NativeResultConfigKey: NativeRouteBinding{
				Version:   1,
				ChannelID: 1,
				Contract:  contract,
			},
			OpenAIImageProfileKey: OpenAIImageProfile{
				Version:        1,
				Enabled:        true,
				ContractSHA256: hex.EncodeToString(sum[:]),
			},
		},
	}
}

func TestOpenAIImageIdentityVirginAndOptionWriteProtection(t *testing.T) {
	db := useImageIdentityDB(t)
	require.NoError(t, InitModelConfigAndChannelCache())
	require.Equal(t, ImageIdentityMissing, LoadModelCaches().ImageIdentityEvidence().State)
	require.True(t, LoadModelCaches().ImageIdentityEvidence().Ready())
	require.False(t, LoadModelCaches().ImageIdentityEvidence().Known(imagecompat.GenerateModel))

	for _, raw := range []string{reviewedEmptyImageIdentities, `{"version":1,"capabilities":{"openai/gpt-image-1.5/text-to-image":["openai/gpt-image-1.5/text-to-image","legacy/stolen"]}}`, "", "broken"} {
		require.Error(t, UpdateOption(OpenAIImageIdentitiesOption, raw))
		require.Error(t, UpdateOptions(map[string]string{OpenAIImageIdentitiesOption: raw}))
	}

	var count int64
	require.NoError(t, db.Model(&Option{}).Count(&count).Error)
	require.Zero(t, count)

	cfg := imageIdentityConfig("vendor/first")
	require.True(t, ValidOpenAIImageProfile(cfg))
	require.NoError(t, SaveModelConfig(cfg))
	stored, err := GetModelConfig(cfg.Model)
	require.NoError(t, err)
	require.True(
		t,
		ValidOpenAIImageProfile(stored),
		"persisted config shares the runtime validator",
	)
	require.True(t, ReadOpenAIImageIdentities(db).Registered(cfg))

	for _, raw := range []string{reviewedEmptyImageIdentities, ""} {
		require.Error(t, UpdateOption(OpenAIImageIdentitiesOption, raw))
	}

	require.True(t, ReadOpenAIImageIdentities(db).Known("vendor/first"))
}

func TestOpenAIImageIdentityMalformedEvidence(t *testing.T) {
	db := useImageIdentityDB(t)
	for _, item := range []struct {
		raw   string
		state ImageIdentityState
	}{
		{`{"version":2,"capabilities":{}}`, ImageIdentityUnsupported},
		{`{"version":1,"version":1,"capabilities":{}}`, ImageIdentityInvalid},
		{`{"version":1,"capabilities":{"unknown/model/image":["unknown/model/image"]}}`, ImageIdentityInvalid},
		{`null`, ImageIdentityInvalid},
	} {
		require.NoError(
			t,
			db.Save(&Option{Key: OpenAIImageIdentitiesOption, Value: item.raw}).Error,
		)
		require.Equal(t, item.state, ReadOpenAIImageIdentities(db).State)
		require.Error(t, SaveModelConfig(imageIdentityConfig("cannot/claim")))
	}
}

func TestOpenAIImageIdentityPersistenceSurvivesRemovalAndRestart(t *testing.T) {
	db := useImageIdentityDB(t)
	cfg := imageIdentityConfig("vendor/old-image")
	require.NoError(t, SaveModelConfig(cfg))
	require.True(t, LoadModelCaches().ImageIdentityEvidence().Registered(cfg))
	delete(cfg.Config, OpenAIImageProfileKey)
	cfg.Config[ModelConfigPublicCapabilityAliasesKey] = []string{"vendor/new-unregistered"}
	require.NoError(t, SaveModelConfig(cfg))
	require.False(t, LoadModelCaches().ImageIdentityEvidence().Known("vendor/new-unregistered"))
	require.NoError(t, DeleteModelConfig(cfg.Model))
	modelCaches.Store(&ModelCaches{})
	require.NoError(t, InitModelConfigAndChannelCache())
	require.True(t, LoadModelCaches().ImageIdentityEvidence().Known("vendor/old-image"))
	require.True(t, LoadModelCaches().ImageIdentityEvidence().Known(imagecompat.GenerateModel))
	require.ErrorContains(
		t,
		SaveModelConfig(ModelConfig{Model: "vendor/old-image", Type: mode.ImagesGenerations}),
		"reserved for historical",
	)
	require.ErrorContains(
		t,
		SaveModelConfig(
			ModelConfig{
				Model:  "other/legacy",
				Type:   mode.ImagesGenerations,
				Config: map[ModelConfigKey]any{ModelConfigPublicModelKey: "VENDOR/OLD-IMAGE"},
			},
		),
		"reserved for historical",
	)
	require.True(t, ReadOpenAIImageIdentities(db).Known("vendor/old-image"))
}

func TestUnacceptedImageProfileCannotAcquireLegacyIdentity(t *testing.T) {
	for _, scenario := range []string{"invalid-profile", "bad-contract", "disabled", "conflict", "legacy-public-name"} {
		t.Run(scenario, func(t *testing.T) {
			db := useImageIdentityDB(t)

			legacy := ModelConfig{Model: "vendor/existing-image", Type: mode.ImagesGenerations}
			if scenario == "legacy-public-name" {
				legacy.Model = "legacy/internal"
				legacy.Config = map[ModelConfigKey]any{
					ModelConfigPublicModelKey: "vendor/existing-image",
				}
			}

			require.NoError(t, SaveModelConfig(legacy))

			candidate := imageIdentityConfig("vendor/existing-image")
			switch scenario {
			case "invalid-profile":
				candidate.Config[OpenAIImageProfileKey] = map[string]any{
					"version": 1,
					"enabled": true,
				}
			case "bad-contract":
				p, ok := candidate.Config[OpenAIImageProfileKey].(OpenAIImageProfile)
				require.True(t, ok)

				p.ContractSHA256 = strings.Repeat("0", 64)
				candidate.Config[OpenAIImageProfileKey] = p
			case "disabled":
				p, ok := candidate.Config[OpenAIImageProfileKey].(OpenAIImageProfile)
				require.True(t, ok)

				p.Enabled = false
				candidate.Config[OpenAIImageProfileKey] = p
			}

			err := SaveModelConfig(candidate)
			if scenario == "conflict" || scenario == "legacy-public-name" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NoError(t, DeleteModelConfig(candidate.Model))
			}

			require.False(t, ReadOpenAIImageIdentities(db).Known("vendor/existing-image"))
			require.False(t, ReadOpenAIImageIdentities(db).Known(imagecompat.GenerateModel))
			require.NoError(t, SaveModelConfig(legacy))
		})
	}
}

func TestOpenAIImageIdentityFailedWriteNeverEnablesConfig(t *testing.T) {
	for _, failure := range []string{"option-insert", "option-append", "model-write"} {
		t.Run(failure, func(t *testing.T) {
			db := useImageIdentityDB(t)
			if failure == "option-append" {
				require.NoError(
					t,
					SaveModelConfig(
						ModelConfig{Model: "legacy/initial", Type: mode.ImagesGenerations},
					),
				)
			}

			if failure == "option-append" {
				require.NoError(
					t,
					db.Callback().
						Update().
						Before("gorm:update").
						Register("fail-append", func(tx *gorm.DB) {
							if tx.Statement.Table == "options" {
								if values, ok := tx.Statement.Dest.(map[string]any); ok {
									if _, isString := values["value"].(string); isString {
										_ = tx.AddError(
											errors.New("synthetic option write failure"),
										)
									}
								}
							}
						}),
				)
			} else {
				require.NoError(
					t,
					db.Callback().
						Create().
						Before("gorm:create").
						Register("fail-create", func(tx *gorm.DB) {
							if (failure == "option-insert" && tx.Statement.Table == "options") ||
								(failure == "model-write" && tx.Statement.Table == "model_configs") {
								_ = tx.AddError(errors.New("synthetic transaction failure"))
							}
						}),
				)
			}

			cfg := imageIdentityConfig("vendor/not-committed")
			require.Error(t, SaveModelConfig(cfg))
			require.False(t, ReadOpenAIImageIdentities(db).Known("vendor/not-committed"))

			var count int64
			require.NoError(
				t,
				db.Model(&ModelConfig{}).Where("model=?", cfg.Model).Count(&count).Error,
			)
			require.Zero(t, count)
		})
	}
}

func TestOpenAIImageIdentityBatchFinalStateAndAtomicFailure(t *testing.T) {
	db := useImageIdentityDB(t)
	legacy := ModelConfig{
		Model:  "legacy/internal",
		Type:   mode.ImagesGenerations,
		Config: map[ModelConfigKey]any{ModelConfigPublicModelKey: "vendor/released-name"},
	}
	require.NoError(t, SaveModelConfig(legacy))

	cfg := imageIdentityConfig("vendor/released-name")
	require.Error(t, SaveModelConfig(cfg))

	legacy.Config = map[ModelConfigKey]any{}
	require.NoError(t, SaveModelConfigs([]ModelConfig{cfg, legacy}))
	require.True(t, ReadOpenAIImageIdentities(db).Known("vendor/released-name"))

	cfg.Config[ModelConfigPublicCapabilityAliasesKey] = []string{"vendor/failed-batch"}
	require.Error(t, SaveModelConfigs([]ModelConfig{cfg, {Model: ""}}))
	require.False(t, ReadOpenAIImageIdentities(db).Known("vendor/failed-batch"))

	stored, err := GetModelConfig(cfg.Model)
	require.NoError(t, err)

	identity, ok := PublicCapabilityIdentityFromConfig(stored)
	require.True(t, ok)
	require.True(t, identity.Accepts("vendor/released-name"))
}

func TestOpenAIImageIdentityFailedRefreshKeepsHistoryClosed(t *testing.T) {
	for _, fault := range []string{"read", "corrupt", "missing"} {
		t.Run(fault, func(t *testing.T) {
			db := useImageIdentityDB(t)
			cfg := imageIdentityConfig("historical/replay")
			require.NoError(t, SaveModelConfig(cfg))

			before := LoadModelCaches()

			var row Option
			require.NoError(t, db.First(&row, "key=?", OpenAIImageIdentitiesOption).Error)

			switch fault {
			case "read":
				require.NoError(
					t,
					db.Callback().
						Query().
						Before("gorm:query").
						Register("failed-config-read", func(tx *gorm.DB) {
							if tx.Statement.Table == "model_configs" {
								_ = tx.AddError(errors.New("synthetic outage"))
							}
						}),
				)
			case "corrupt":
				require.NoError(
					t,
					db.Model(&Option{}).Where("key=?", row.Key).Update("value", "broken").Error,
				)
			case "missing":
				require.NoError(t, db.Delete(&row).Error)
			}

			require.Error(t, InitModelConfigAndChannelCache())
			require.Same(t, before, LoadModelCaches())
			evidence := LoadModelCaches().ImageIdentityEvidence()
			require.Equal(t, ImageIdentityUnavailable, evidence.State)
			require.True(t, evidence.Trusted())
			require.False(t, evidence.Ready())
			require.True(t, evidence.Known("historical/replay"))
			require.False(t, evidence.Known("legacy/unrelated"))

			if fault == "read" {
				require.NoError(t, db.Callback().Query().Remove("failed-config-read"))
			} else {
				require.NoError(t, db.Save(&row).Error)
			}

			next := imageIdentityConfig("new/while-untrusted")
			require.Error(t, SaveModelConfig(next), "refresh must recover before new activation")
			require.NoError(t, InitModelConfigAndChannelCache())
			require.NoError(t, SaveModelConfig(next))
			require.True(t, LoadModelCaches().ImageIdentityEvidence().Known("historical/replay"))
			require.True(t, LoadModelCaches().ImageIdentityEvidence().Registered(next))
		})
	}
}

func installImageIdentityMigrationFixture(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(
		t,
		db.AutoMigrate(
			&ChannelAdmissionControl{},
			&ChannelAdmissionProof{},
			&ChannelAdmissionFamily{},
			&ChannelAdmissionBarrier{},
			&ChannelAdmissionUse{},
			&ChannelMigrationProtocol{},
			&ChannelMigrationPrepared{},
			&ChannelMigrationProbeRecord{},
		),
	)
	require.NoError(t, db.Create(&ChannelAdmissionControl{ID: 1, Version: 1}).Error)
	require.NoError(t, db.Create(&ChannelMigrationProtocol{ID: 1, Version: 1}).Error)
}

func imageIdentityConcurrentClaims(t *testing.T) {
	t.Helper()

	const n = 6

	start := make(chan struct{})

	var wg sync.WaitGroup

	results := make(chan struct {
		id  string
		err error
	}, n)
	for i := range n {
		alias := fmt.Sprintf("concurrent/image-%d", i)
		wg.Go(func() {
			<-start

			results <- struct {
				id  string
				err error
			}{alias, SaveModelConfig(imageIdentityConfig(alias))}
		})
	}

	close(start)
	wg.Wait()
	close(results)

	evidence := ReadOpenAIImageIdentities(DB)

	success := 0
	for result := range results {
		if result.err == nil {
			success++

			require.True(
				t,
				evidence.Known(result.id),
				"successful claim must survive later config replacement",
			)
		}
	}

	require.Positive(t, success)

	if DB.Name() == "postgres" {
		require.Equal(t, n, success)
	}
}

func imageIdentityConcurrentLegacy(t *testing.T) {
	t.Helper()

	for i := range 4 {
		alias := fmt.Sprintf("race/legacy-%d", i)
		cfg := imageIdentityConfig(alias)
		legacy := ModelConfig{Model: alias, Type: mode.ImagesGenerations}
		start := make(chan struct{})

		result := make(chan error, 2)
		go func() { <-start; result <- SaveModelConfig(cfg) }()
		go func() { <-start; result <- SaveModelConfig(legacy) }()

		close(start)

		first, second := <-result, <-result
		require.False(
			t,
			first == nil && second == nil,
			"claim and colliding legacy cannot both commit",
		)
		require.True(t, first == nil || second == nil, "one contender must make progress")

		var count int64
		require.NoError(t, DB.Model(&ModelConfig{}).Where("model=?", alias).Count(&count).Error)
		require.False(t, ReadOpenAIImageIdentities(DB).Known(alias) && count != 0)
	}
}

func TestOpenAIImageIdentityConcurrentAppendsPreserveAllSuccessfulIDs(t *testing.T) {
	for _, extension := range []bool{false, true} {
		t.Run(strconv.FormatBool(extension), func(t *testing.T) {
			db := useImageIdentityDB(t)
			if extension {
				installImageIdentityMigrationFixture(t, db)
			}

			imageIdentityConcurrentClaims(t)
			imageIdentityConcurrentLegacy(t)
		})
	}
}

func TestOpenAIImageIdentityPostgresPersistence(t *testing.T) {
	for _, extension := range []bool{false, true} {
		t.Run(strconv.FormatBool(extension), func(t *testing.T) {
			withTestPostgresStoreDB(t, func() {
				useImageIdentityDBOn(t, DB)

				if extension {
					installImageIdentityMigrationFixture(t, DB)
				}

				imageIdentityConcurrentClaims(t)
				imageIdentityConcurrentLegacy(t)

				cfg := imageIdentityConfig("pg/config-identity")
				require.NoError(t, SaveModelConfig(cfg))
				require.NoError(t, DeleteModelConfig(cfg.Model))
				modelCaches.Store(&ModelCaches{})
				require.NoError(t, InitModelConfigAndChannelCache())
				require.True(
					t,
					LoadModelCaches().ImageIdentityEvidence().Known("pg/config-identity"),
				)
			})
		})
	}
}

func TestOpenAIImageIdentityBatchDuplicateKeyUsesFinalConfig(t *testing.T) {
	db := useImageIdentityDB(t)
	candidate := imageIdentityConfig("batch/never-activated")
	final := imageIdentityConfig("batch/disabled-final")
	delete(final.Config, OpenAIImageProfileKey)
	require.NoError(t, SaveModelConfigs([]ModelConfig{candidate, final}))

	evidence := ReadOpenAIImageIdentities(db)
	require.False(t, evidence.Known("batch/never-activated"))
	require.False(t, evidence.Known("batch/disabled-final"))
	require.False(t, evidence.Known(imagecompat.GenerateModel))

	stored, err := GetModelConfig(final.Model)
	require.NoError(t, err)
	require.False(t, ValidOpenAIImageProfile(stored))
}

func TestOpenAIImageIdentityColdUnregisteredProfileRequiresRecovery(t *testing.T) {
	db := useImageIdentityDB(t)
	cfg := imageIdentityConfig("unsupported/old-adapter")
	// Simulate a partial restore/older adapter that bypassed the normal configuration transaction.
	require.NoError(t, db.Create(&cfg).Error)
	require.NoError(t, InitModelConfigAndChannelCache())

	evidence := LoadModelCaches().ImageIdentityEvidence()
	require.Equal(t, ImageIdentityInvalid, evidence.State)
	require.False(t, evidence.Ready())
	require.False(t, evidence.Trusted())
	require.Error(
		t,
		SaveModelConfig(cfg),
		"normal save must not silently repair unknown admission history",
	)
	require.Equal(t, ImageIdentityMissing, ReadOpenAIImageIdentities(db).State)
}
