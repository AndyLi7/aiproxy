//nolint:testpackage
package model

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const rebuildTestModel = "vendor/rebuild-model"

func useCacheRebuildDB(t *testing.T) (*gorm.DB, *Channel) {
	t.Helper()
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "rebuild.db"))
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)

	previousDB, previousCaches := DB, LoadModelCaches()
	DB = db
	t.Cleanup(func() {
		DB = previousDB

		modelCaches.Store(previousCaches)
		require.NoError(t, sqlDB.Close())
	})
	// A coherent cache read now holds a read transaction. WAL lets this test
	// commit the writer while that old snapshot remains paused, preserving its
	// original overwrite race. Production DSNs are unchanged.
	require.NoError(t, db.Exec("PRAGMA journal_mode=WAL").Error)
	require.NoError(t, db.AutoMigrate(&Channel{}, &ModelConfig{}))
	require.NoError(
		t,
		db.Create(&ModelConfig{Model: rebuildTestModel, Type: mode.NativeTasks}).Error,
	)
	channel := &Channel{
		Name:   "rebuild",
		Type:   ChannelTypeFal,
		Key:    "k",
		Models: []string{rebuildTestModel},
	}
	require.NoError(t, db.Create(channel).Error)
	require.NoError(t, InitModelConfigAndChannelCache())

	return db, channel
}

func cachedChannelEnabled(caches *ModelCaches, id int) bool {
	for _, channel := range caches.EnabledModel2ChannelsBySet[ChannelDefaultSet][rebuildTestModel] {
		if channel.ID == id {
			return true
		}
	}

	return false
}

// A ticker rebuild that read the channels before a retire must not store its
// snapshot after the retire's own rebuild: the retired channel would stay
// routable until the next tick.
func TestCacheRebuildStartedBeforeWriteCannotOverwriteIt(t *testing.T) {
	db, channel := useCacheRebuildDB(t)
	require.True(t, cachedChannelEnabled(LoadModelCaches(), channel.ID))
	start := LoadModelCaches().Version

	var (
		armed       atomic.Bool
		channelRead atomic.Int64
	)

	reached, release := make(chan struct{}), make(chan struct{})
	require.NoError(
		t,
		db.Callback().
			Query().
			After("gorm:query").
			Register("test:hold_channel_read", func(tx *gorm.DB) {
				if tx.Statement.Table != "channels" {
					return
				}

				channelRead.Add(1)

				if armed.CompareAndSwap(true, false) {
					close(reached)
					<-release
				}
			}),
	)

	// The ticker rebuild reads the channel while it is still enabled, then stalls.
	armed.Store(true)

	ticker := make(chan error, 1)
	go func() { ticker <- InitModelConfigAndChannelCache() }()

	<-reached

	// The retire commits and rebuilds.
	retire := make(chan error, 1)
	go func() { retire <- UpdateChannelStatusByID(channel.ID, ChannelStatusDisabled) }()

	require.Eventually(t, func() bool {
		stored, err := GetChannelByID(channel.ID)
		return err == nil && stored.Status == ChannelStatusDisabled
	}, 5*time.Second, 5*time.Millisecond)
	// Give an unserialized rebuild the chance to finish first (it must not).
	reads := channelRead.Load()

	time.Sleep(100 * time.Millisecond)
	require.Equal(
		t,
		reads,
		channelRead.Load(),
		"the retire's rebuild ran while another rebuild was in progress",
	)

	close(release)
	require.NoError(t, <-ticker)
	require.NoError(t, <-retire)

	caches := LoadModelCaches()
	require.False(
		t,
		cachedChannelEnabled(caches, channel.ID),
		"an older rebuild overwrote the retire",
	)
	require.Equal(t, ChannelStatusDisabled, caches.ChannelsByID[channel.ID].Status)
	require.Equal(t, start+2, caches.Version)
	require.False(t, caches.BuiltAt.IsZero())
}

// Ticker rebuilds and admin writes run concurrently (run with -race): every
// stored version is newer than the one before, and the last write wins.
func TestConcurrentCacheRebuildsAreSerialized(t *testing.T) {
	_, channel := useCacheRebuildDB(t)
	start := LoadModelCaches().Version

	var (
		rebuilds atomic.Uint64
		wg       sync.WaitGroup
	)

	stop := make(chan struct{})

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)

		last := LoadModelCaches().Version
		for {
			select {
			case <-stop:
				return
			default:
			}

			current := LoadModelCaches().Version
			if current < last {
				t.Errorf("cache version went back from %d to %d", last, current)
				return
			}

			last = current
		}
	}()

	for worker := range 6 {
		wg.Go(func() {
			for i := range 10 {
				if worker%2 == 0 {
					if err := InitModelConfigAndChannelCache(); err != nil {
						t.Errorf("rebuild: %v", err)
						return
					}
				} else {
					status := ChannelStatusEnabled
					if i%2 == 0 {
						status = ChannelStatusDisabled
					}

					if err := UpdateChannelStatusByID(channel.ID, status); err != nil {
						t.Errorf("status write: %v", err)
						return
					}
				}

				rebuilds.Add(1)
			}
		})
	}

	wg.Wait()
	close(stop)
	<-readerDone

	require.NoError(t, UpdateChannelStatusByID(channel.ID, ChannelStatusEnabled))
	rebuilds.Add(1)

	caches := LoadModelCaches()
	require.True(t, cachedChannelEnabled(caches, channel.ID))
	require.Equal(t, start+rebuilds.Load(), caches.Version)
}
