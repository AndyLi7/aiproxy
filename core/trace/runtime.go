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
	"gorm.io/gorm/logger"
)

const (
	initializationTimeout = 5 * time.Second
	cleanupInterval       = time.Hour
	cleanupTimeout        = 10 * time.Second
	cleanupBatchSize      = 500
)

type Options struct {
	Enabled     bool
	TrustedKeys map[string][]byte
}

type Health struct {
	CorrelationFailures  uint64                   `json:"correlation_failures"`
	Enabled              bool                     `json:"enabled"`
	Ready                bool                     `json:"ready"`
	Writer               requesttrace.WriterStats `json:"writer"`
	CleanupErrors        uint64                   `json:"cleanup_errors"`
	InitializationFailed bool                     `json:"initialization_failed"`
}

type Runtime struct {
	correlationFailures  atomic.Uint64
	trustedKeys          map[string][]byte
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

	traceDB := db
	if db != nil {
		traceDB = db.Session(&gorm.Session{NewDB: true, Logger: logger.Discard})
	}
	store := model.NewTraceStore(traceDB)
	migrateCtx, cancelMigrate := context.WithTimeout(ctx, initializationTimeout)
	err := store.Migrate(migrateCtx)
	cancelMigrate()
	if err != nil {
		runtime.initializationFailed = true
		log.Error("request_trace_initialization_failed")
		return runtime
	}

	runtime.ready = true
	runtime.trustedKeys = make(map[string][]byte, len(options.TrustedKeys))
	for id, key := range options.TrustedKeys {
		runtime.trustedKeys[id] = append([]byte(nil), key...)
	}
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

// BindRequest keeps a local trace whenever authenticated correlation is unavailable.
func (r *Runtime) BindRequest(ctx context.Context, session *requesttrace.Session, wire string, actual requesttrace.TrustedRequest) bool {
	if session == nil {
		return false
	}
	if r != nil && r.ready && wire != "" {
		verified, err := requesttrace.VerifyTrustedContext(ctx, wire, r.trustedKeys, actual, time.Now(), r.store)
		if err == nil && session.BindVerifiedContext(actual.GroupID, verified) {
			return true
		}
		r.correlationFailures.Add(1)
	}
	return session.BindGroup(actual.GroupID)
}

func (r *Runtime) SaveTask(ctx context.Context, session *requesttrace.Session, asyncID int, group string) {
	if r == nil || !r.ready || session == nil {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	// Diagnostic persistence must never alter the successful task or billing path.
	if r.store.SaveTaskTrace(bounded, asyncID, group, session.TraceID(), session.RootID()) != nil {
		r.correlationFailures.Add(1)
	}
}

func (r *Runtime) BindTask(ctx context.Context, session *requesttrace.Session, group string, tokenID, channelID int, taskID string) bool {
	if r == nil || !r.ready || session == nil {
		return false
	}
	bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	link, err := r.store.ResolveTaskTrace(bounded, group, tokenID, channelID, taskID, time.Now())
	if err != nil {
		r.correlationFailures.Add(1)
		return session.BindGroup(group)
	}
	return session.BindStoredTask(group, link.TraceID, link.ParentSpanID)
}

func (r *Runtime) Health() Health {
	if r == nil {
		return Health{}
	}
	health := Health{
		CorrelationFailures:  r.correlationFailures.Load(),
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
			_ = writer.Close(ctx)
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
			_, nonceErr := r.store.CleanTraceNonces(cleanupCtx, time.Now().UTC(), cleanupBatchSize)
			_, taskErr := r.store.CleanTaskTraces(cleanupCtx, time.Now().UTC(), cleanupBatchSize)
			cancel()
			if (err != nil || nonceErr != nil || taskErr != nil) && ctx.Err() == nil {
				r.cleanupErrors.Add(1)
				log.Error("request_trace_cleanup_failed")
			}
		}
	}
}
