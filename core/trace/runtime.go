// Package trace manages the opt-in request trace persistence runtime.
package trace

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

const (
	initializationTimeout = 5 * time.Second
	cleanupInterval       = time.Hour
	cleanupTimeout        = 10 * time.Second
	cleanupBatchSize      = 500
)

type Options struct {
	Enabled bool
}

type Health struct {
	Enabled              bool                     `json:"enabled"`
	Ready                bool                     `json:"ready"`
	Writer               requesttrace.WriterStats `json:"writer"`
	CleanupErrors        uint64                   `json:"cleanup_errors"`
	InitializationFailed bool                     `json:"initialization_failed"`
}

type Runtime struct {
	enabled              bool
	ready                bool
	initializationFailed bool
	store                *model.TraceStore
	writer               *requesttrace.Writer
	cleanupErrors        atomic.Uint64
	cleanupCancel        context.CancelFunc
	cleanupDone          chan struct{}

	closeMu sync.Mutex
	closed  bool
}

var current atomic.Pointer[Runtime]

var newCleanupTicker = func(interval time.Duration) (<-chan time.Time, func()) {
	ticker := time.NewTicker(interval)
	return ticker.C, ticker.Stop
}

func Start(ctx context.Context, db *gorm.DB, options Options) *Runtime {
	runtime := &Runtime{enabled: options.Enabled}
	if !options.Enabled {
		return runtime
	}
	if ctx == nil {
		ctx = context.Background()
	}

	store := model.NewTraceStore(db)
	migrateCtx, cancelMigrate := context.WithTimeout(ctx, initializationTimeout)
	err := store.Migrate(migrateCtx)
	cancelMigrate()
	if err != nil {
		runtime.initializationFailed = true
		log.Error("request_trace_initialization_failed")
		return runtime
	}

	runtime.ready = true
	runtime.store = store
	runtime.writer = requesttrace.NewWriter(store, requesttrace.WriterOptions{})
	cleanupCtx, cancelCleanup := context.WithCancel(ctx)
	runtime.cleanupCancel = cancelCleanup
	runtime.cleanupDone = make(chan struct{})
	go runtime.runCleanup(cleanupCtx)
	return runtime
}

func (r *Runtime) NewRequest(requestID string) *requesttrace.Session {
	if r == nil || !r.ready || r.writer == nil {
		return nil
	}
	return requesttrace.NewSession(requestID, r.writer.Submit)
}

func (r *Runtime) Health() Health {
	if r == nil {
		return Health{}
	}
	health := Health{
		Enabled:              r.enabled,
		Ready:                r.ready,
		CleanupErrors:        r.cleanupErrors.Load(),
		InitializationFailed: r.initializationFailed,
	}
	if r.writer != nil {
		health.Writer = r.writer.Stats()
	}
	return health
}

func (r *Runtime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	r.closeMu.Lock()
	if !r.closed {
		r.closed = true
		if r.cleanupCancel != nil {
			r.cleanupCancel()
		}
	}
	cleanupDone := r.cleanupDone
	writer := r.writer
	r.closeMu.Unlock()

	if cleanupDone != nil {
		select {
		case <-cleanupDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return writer.Close(ctx)
}

func Current() *Runtime {
	return current.Load()
}

func Install(runtime *Runtime) (restore func()) {
	previous := current.Swap(runtime)
	return func() { current.Store(previous) }
}

func (r *Runtime) runCleanup(ctx context.Context) {
	defer close(r.cleanupDone)
	ticks, stop := newCleanupTicker(cleanupInterval)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			cleanupCtx, cancel := context.WithTimeout(ctx, cleanupTimeout)
			_, err := r.store.CleanExpired(cleanupCtx, time.Now().UTC(), cleanupBatchSize)
			cancel()
			if err != nil && ctx.Err() == nil {
				r.cleanupErrors.Add(1)
				log.Error("request_trace_cleanup_failed")
			}
		}
	}
}
