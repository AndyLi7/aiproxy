package model

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/notify"
	"github.com/labring/aiproxy/core/common/oncall"
	"github.com/maruel/natural"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

type ModelConfigCache interface {
	GetModelConfig(model string) (ModelConfig, bool)
}

type ModelCaches struct {
	OpenAIImageIdentities   OpenAIImageIdentitySnapshot
	imageIdentityReadFailed *atomic.Bool
	ChannelsByID            map[int]*Channel

	ModelConfig ModelConfigCache

	EnabledModelsBySet       map[string][]string
	EnabledModelConfigsBySet map[string][]ModelConfig
	EnabledModelConfigsMap   map[string]ModelConfig

	EnabledModel2ChannelsBySet  map[string]map[string][]*Channel
	DisabledModel2ChannelsBySet map[string]map[string][]*Channel

	// Version increases by one with every rebuild stored by this process
	// (0 = never rebuilt); BuiltAt is when that rebuild was stored. Logs name
	// the routing state a request was served from.
	Version uint64
	BuiltAt time.Time

	// Published together with this exact routing pointer; never an all-replica ACK.
	migrationSource *migrationCacheSource
}

var (
	modelCaches atomic.Pointer[ModelCaches]

	// cacheRebuildMu serializes rebuilds from the first database read to the
	// store, and guards cacheVersion. The 10 s ticker and the rebuild after
	// every admin write used to run concurrently, so a rebuild that read the
	// database before a write could store its snapshot after the write's own
	// rebuild (for example keeping a retired channel routable for up to 10 s).
	// Serialized, stores happen in read order: the last store is the newest.
	cacheRebuildMu sync.Mutex
	cacheVersion   uint64
)

func init() {
	modelCaches.Store(new(ModelCaches))
}

func LoadModelCaches() *ModelCaches {
	return modelCaches.Load()
}

func InitModelConfigAndChannelCache() error {
	cacheRebuildMu.Lock()
	defer cacheRebuildMu.Unlock()

	snapshot, err := readCacheSnapshot()
	if err != nil {
		if fault := modelCaches.Load().imageIdentityReadFailed; fault != nil {
			fault.Store(true)
		}
		return err
	}

	captureMigrationCacheRouting(&snapshot)
	modelConfig := applyYAMLConfigToModelConfigCache(newModelConfigCache(snapshot.modelConfigs))

	// Channel models are filtered against the configs read above, not against
	// a fresh query per channel: one rebuild sees one set of model configs.
	configured := configuredModels(snapshot.modelConfigs)
	enabledChannels := prepareCachedChannels(
		snapshot.enabledChannels,
		ChannelStatusEnabled,
		configured,
	)

	disabledChannels := prepareCachedChannels(
		snapshot.disabledChannels,
		ChannelStatusDisabled,
		configured,
	)
	if source := snapshot.migrationSource; source != nil &&
		(len(enabledChannels) != len(snapshot.enabledChannels) || len(disabledChannels) != len(snapshot.disabledChannels)) {
		source.reason = "effective_cache_projection_unsupported"
	}

	enabledModel2ChannelsBySet := buildModelToChannelsBySetMap(enabledChannels)
	sortChannelsByPriorityBySet(enabledModel2ChannelsBySet)

	enabledModelsBySet, enabledModelConfigsBySet, enabledModelConfigsMap := buildEnabledModelsBySet(
		enabledModel2ChannelsBySet,
		modelConfig,
	)

	addPrivateAgentInferenceModels(
		snapshot.modelConfigs,
		enabledModelsBySet,
		enabledModelConfigsBySet,
		enabledModelConfigsMap,
	)

	disabledModel2ChannelsBySet := buildModelToChannelsBySetMap(disabledChannels)

	channelsByID := make(map[int]*Channel, len(enabledChannels)+len(disabledChannels))
	for _, channels := range [][]*Channel{enabledChannels, disabledChannels} {
		for _, channel := range channels {
			channelsByID[channel.ID] = channel
		}
	}

	identityConfigs := append([]ModelConfig(nil), snapshot.modelConfigs...)
	if yaml := LoadYAMLConfig(); yaml != nil {
		for _, item := range yaml.ModelConfigs {
			if effective, ok := modelConfig.GetModelConfig(item.Model); ok {
				identityConfigs = append(identityConfigs, effective)
			}
		}
	}

	identities := snapshot.imageIdentities.withCurrent(identityConfigs)

	previous := modelCaches.Load()
	if !identities.Ready() || !identities.retains(previous.OpenAIImageIdentities) {
		if previous.OpenAIImageIdentities.Trusted() {
			if fault := previous.imageIdentityReadFailed; fault != nil {
				fault.Store(true)
			}
			return errImageIdentityInvalid
		}
	}
	cacheVersion++
	next := &ModelCaches{
		OpenAIImageIdentities:   identities,
		imageIdentityReadFailed: &atomic.Bool{},
		ChannelsByID:            channelsByID,

		ModelConfig: modelConfig,

		EnabledModelsBySet:       enabledModelsBySet,
		EnabledModelConfigsBySet: enabledModelConfigsBySet,
		EnabledModelConfigsMap:   enabledModelConfigsMap,

		EnabledModel2ChannelsBySet:  enabledModel2ChannelsBySet,
		DisabledModel2ChannelsBySet: disabledModel2ChannelsBySet,

		Version: cacheVersion,
	}
	next.migrationSource = finishMigrationCacheSource(snapshot, next)
	next.BuiltAt = time.Now()
	modelCaches.Store(next)

	return nil
}

// rebuildModelCachesAfterWrite refreshes the caches after a committed admin
// write. The write stands either way and the ticker retries within seconds,
// so a failed rebuild is not the writer's error; it used to be discarded and
// is now logged (throttled, like the ticker's own failures).
func rebuildModelCachesAfterWrite() {
	if err := InitModelConfigAndChannelCache(); err != nil {
		notify.ErrorThrottle(
			"rebuildModelCachesAfterWrite",
			time.Minute*5,
			"failed to rebuild model and channel caches after a write",
			err.Error(),
		)
	}
}

// cacheSnapshot is what one rebuild reads from the database.
type cacheSnapshot struct {
	imageIdentities  OpenAIImageIdentitySnapshot
	modelConfigs     []ModelConfig
	enabledChannels  []*Channel
	disabledChannels []*Channel
	migrationSource  *migrationCacheSource
}

// readCacheSnapshot reads model configs and channels. On PostgreSQL the reads
// share one REPEATABLE READ, READ ONLY transaction, so the cache reflects one
// point in time: a write committed between two reads (a model config deleted
// while a channel listing it is re-pointed, say) can no longer leave a cache
// mixing both states. SQLite uses one read transaction as well.
func readCacheSnapshot() (cacheSnapshot, error) {
	var snapshot cacheSnapshot

	read := func(tx *gorm.DB) error {
		var err error

		snapshot.migrationSource, err = readMigrationCacheSource(tx)
		if err != nil {
			return err
		}

		if err := tx.Model(&ModelConfig{}).
			Order("created_at desc").
			Find(&snapshot.modelConfigs).
			Error; err != nil {
			return err
		}

		if err := tx.Where("status = ?", ChannelStatusEnabled).
			Find(&snapshot.enabledChannels).
			Error; err != nil {
			return err
		}

		if err := tx.Where("status = ?", ChannelStatusDisabled).
			Find(&snapshot.disabledChannels).
			Error; err != nil {
			return err
		}

		snapshot.imageIdentities = ReadOpenAIImageIdentities(tx)

		return nil
	}

	if DB.Name() == "sqlite" {
		err := DB.Transaction(read, &sql.TxOptions{ReadOnly: true})
		return snapshot, err
	}

	if DB.Name() != "postgres" {
		return snapshot, read(DB)
	}

	err := DB.Transaction(read, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})

	return snapshot, err
}

// configuredModels is the set of models with a stored config, or nil while
// model configs are disabled (every channel model is then kept).
func configuredModels(configs []ModelConfig) map[string]struct{} {
	if config.DisableModelConfig {
		return nil
	}

	models := make(map[string]struct{}, len(configs))
	for _, modelConfig := range configs {
		models[modelConfig.Model] = struct{}{}
	}

	return models
}

// prepareCachedChannels is LoadEnabledChannels/LoadDisabledChannels for
// channels already read: YAML channels of that status are added, and every
// channel's models and mapping are initialized against configured.
func prepareCachedChannels(
	channels []*Channel,
	status int,
	configured map[string]struct{},
) []*Channel {
	configChannels := NewConfigChannels(LoadYAMLConfig(), status)
	if len(configChannels) != 0 {
		log.Infof("added %d channels from config", len(configChannels))
		channels = append(channels, configChannels...)
	}

	for _, channel := range channels {
		initializeChannelModelsFrom(channel, configured)
		initializeChannelModelMapping(channel)
	}

	return channels
}

func LoadEnabledChannels() ([]*Channel, error) {
	var channels []*Channel

	err := DB.Where("status = ?", ChannelStatusEnabled).Find(&channels).Error
	if err != nil {
		return nil, err
	}

	configChannels := NewConfigChannels(LoadYAMLConfig(), ChannelStatusEnabled)
	if len(configChannels) != 0 {
		log.Infof("added %d channels from config", len(configChannels))
		channels = append(channels, configChannels...)
	}

	for _, channel := range channels {
		initializeChannelModels(channel)
		initializeChannelModelMapping(channel)
	}

	return channels, nil
}

func LoadDisabledChannels() ([]*Channel, error) {
	var channels []*Channel

	err := DB.Where("status = ?", ChannelStatusDisabled).Find(&channels).Error
	if err != nil {
		return nil, err
	}

	configChannels := NewConfigChannels(LoadYAMLConfig(), ChannelStatusDisabled)
	if len(configChannels) != 0 {
		log.Infof("added %d channels from config", len(configChannels))
		channels = append(channels, configChannels...)
	}

	for _, channel := range channels {
		initializeChannelModels(channel)
		initializeChannelModelMapping(channel)
	}

	return channels, nil
}

func LoadChannels() ([]*Channel, error) {
	var channels []*Channel

	err := DB.Find(&channels).Error
	if err != nil {
		return nil, err
	}

	configChannels := NewConfigChannels(LoadYAMLConfig(), 0)
	if len(configChannels) != 0 {
		log.Infof("added %d channels from config", len(configChannels))
		channels = append(channels, configChannels...)
	}

	for _, channel := range channels {
		initializeChannelModels(channel)
		initializeChannelModelMapping(channel)
	}

	return channels, nil
}

func LoadChannelByID(id int) (*Channel, error) {
	var channel Channel

	err := DB.First(&channel, id).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}

		chs, err := LoadChannels()
		if err != nil {
			return nil, err
		}

		for _, c := range chs {
			if c.ID == id {
				return c, nil
			}
		}

		return nil, gorm.ErrRecordNotFound
	}

	initializeChannelModels(&channel)
	initializeChannelModelMapping(&channel)

	return &channel, nil
}

var _ ModelConfigCache = (*modelConfigMapCache)(nil)

type modelConfigMapCache struct {
	modelConfigMap map[string]ModelConfig
}

func (m *modelConfigMapCache) GetModelConfig(model string) (ModelConfig, bool) {
	config, ok := m.modelConfigMap[model]
	return config, ok
}

var _ ModelConfigCache = (*disabledModelConfigCache)(nil)

type disabledModelConfigCache struct {
	modelConfigs ModelConfigCache
}

func (d *disabledModelConfigCache) GetModelConfig(model string) (ModelConfig, bool) {
	if config, ok := d.modelConfigs.GetModelConfig(model); ok {
		return config, true
	}
	return NewDefaultModelConfig(model), true
}

func newModelConfigCache(modelConfigs []ModelConfig) ModelConfigCache {
	newModelConfigMap := make(map[string]ModelConfig)
	for _, modelConfig := range modelConfigs {
		newModelConfigMap[modelConfig.Model] = modelConfig
	}

	configs := &modelConfigMapCache{modelConfigMap: newModelConfigMap}
	if config.DisableModelConfig {
		return &disabledModelConfigCache{modelConfigs: configs}
	}

	return configs
}

func initializeChannelModels(channel *Channel) {
	if len(channel.Models) == 0 {
		channel.Models = config.GetDefaultChannelModels()[int(channel.Type)]
		return
	}

	findedModels, missingModels, err := GetModelConfigWithModels(channel.Models)
	if err != nil {
		return
	}

	if len(missingModels) > 0 {
		slices.Sort(missingModels)
		log.Errorf("model config not found: %v", missingModels)
	}

	slices.Sort(findedModels)
	channel.Models = findedModels
}

// initializeChannelModelsFrom is initializeChannelModels against configs
// already read (configured, nil while model configs are disabled): an empty
// list falls back to the type's default models; otherwise models without a
// config are dropped and logged, and the rest are kept once each, sorted.
func initializeChannelModelsFrom(channel *Channel, configured map[string]struct{}) {
	if len(channel.Models) == 0 {
		channel.Models = config.GetDefaultChannelModels()[int(channel.Type)]
		return
	}

	if configured == nil {
		slices.Sort(channel.Models)
		return
	}

	var found, missing []string

	seen := make(map[string]struct{}, len(channel.Models))
	for _, model := range channel.Models {
		if _, ok := seen[model]; ok {
			continue
		}

		seen[model] = struct{}{}

		if _, ok := configured[model]; ok {
			found = append(found, model)
		} else {
			missing = append(missing, model)
		}
	}

	if len(missing) > 0 {
		slices.Sort(missing)
		log.Errorf("model config not found: %v", missing)
	}

	slices.Sort(found)
	channel.Models = found
}

func initializeChannelModelMapping(channel *Channel) {
	if len(channel.ModelMapping) == 0 {
		channel.ModelMapping = config.GetDefaultChannelModelMapping()[int(channel.Type)]
	}
}

func buildModelToChannelsBySetMap(channels []*Channel) map[string]map[string][]*Channel {
	modelMapBySet := make(map[string]map[string][]*Channel)

	for _, channel := range channels {
		sets := channel.GetSets()
		for _, set := range sets {
			if _, ok := modelMapBySet[set]; !ok {
				modelMapBySet[set] = make(map[string][]*Channel)
			}

			for _, model := range channel.Models {
				modelMapBySet[set][model] = append(modelMapBySet[set][model], channel)
			}
		}
	}

	return modelMapBySet
}

func sortChannelsByPriorityBySet(modelMapBySet map[string]map[string][]*Channel) {
	for _, modelMap := range modelMapBySet {
		for _, channels := range modelMap {
			sort.Slice(channels, func(i, j int) bool {
				return channels[i].GetPriority() > channels[j].GetPriority()
			})
		}
	}
}

func buildEnabledModelsBySet(
	modelMapBySet map[string]map[string][]*Channel,
	modelConfigCache ModelConfigCache,
) (
	map[string][]string,
	map[string][]ModelConfig,
	map[string]ModelConfig,
) {
	modelsBySet := make(map[string][]string)
	modelConfigsBySet := make(map[string][]ModelConfig)
	modelConfigsMap := make(map[string]ModelConfig)

	for set, modelMap := range modelMapBySet {
		models := make([]string, 0)
		configs := make([]ModelConfig, 0)
		appended := make(map[string]struct{})

		for model := range modelMap {
			if _, ok := appended[model]; ok {
				continue
			}

			if config, ok := modelConfigCache.GetModelConfig(model); ok {
				models = append(models, model)
				configs = append(configs, config)
				appended[model] = struct{}{}
				modelConfigsMap[model] = config
			}
		}

		slices.Sort(models)
		slices.SortStableFunc(configs, SortModelConfigsFunc)

		modelsBySet[set] = models
		modelConfigsBySet[set] = configs
	}

	return modelsBySet, modelConfigsBySet, modelConfigsMap
}

func SortModelConfigsFunc(i, j ModelConfig) int {
	if i.Owner != j.Owner {
		if natural.Less(string(i.Owner), string(j.Owner)) {
			return -1
		}
		return 1
	}

	if i.Type != j.Type {
		if i.Type < j.Type {
			return -1
		}
		return 1
	}

	if i.Model == j.Model {
		return 0
	}

	if natural.Less(i.Model, j.Model) {
		return -1
	}

	return 1
}

func SyncModelConfigAndChannelCache(
	ctx context.Context,
	wg *sync.WaitGroup,
	frequency time.Duration,
) {
	defer wg.Done()

	ticker := time.NewTicker(frequency)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := InitModelConfigAndChannelCache()
			if err != nil {
				notify.ErrorThrottle(
					"syncModelChannel",
					time.Minute*5,
					"failed to sync channels",
					err.Error(),
				)
				oncall.AlertDBError("SyncModelConfigAndChannelCache", err)
			} else {
				oncall.ClearDBError("SyncModelConfigAndChannelCache")
			}
		}
	}
}
