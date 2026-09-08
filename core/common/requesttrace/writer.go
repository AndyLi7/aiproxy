package requesttrace

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultWriterQueueSize    = 4096
	maxWriterQueueSize        = 4096
	defaultWriterWriteTimeout = time.Second
	defaultWriterMaxAttempts  = 2
	maxWriterMaxAttempts      = 2
	writerRetryBackoff        = 50 * time.Millisecond
)

// Sink persists one trace span. Write must honor ctx cancellation and must not
// retain or mutate span after returning.
type Sink interface {
	Write(context.Context, Span) error
}

type WriterOptions struct {
	QueueSize    int
	WriteTimeout time.Duration
	MaxAttempts  int
}

// WriterStats is a point-in-time snapshot of writer outcomes. Accepted counts
// spans admitted to the queue. Rejected counts invalid spans and submissions
// made to a disabled or closed writer. Dropped counts valid spans refused by a
// full queue plus accepted spans lost to exhausted writes or canceled shutdown.
// Persisted counts successful sink writes, and WriteErrors counts failed sink
// calls (including calls canceled during shutdown). These categories are not
// all disjoint: an accepted span can later be counted as dropped.
type WriterStats struct {
	Accepted    uint64
	Persisted   uint64
	Rejected    uint64
	Dropped     uint64
	WriteErrors uint64
}

type Writer struct {
	sink         Sink
	queue        chan Span
	writeTimeout time.Duration
	maxAttempts  int

	mu     sync.Mutex
	closed bool
	cancel context.CancelFunc
	done   chan struct{}

	accepted    atomic.Uint64
	persisted   atomic.Uint64
	rejected    atomic.Uint64
	dropped     atomic.Uint64
	writeErrors atomic.Uint64
}

func NewWriter(sink Sink, options WriterOptions) *Writer {
	queueSize := options.QueueSize
	if queueSize <= 0 {
		queueSize = defaultWriterQueueSize
	} else if queueSize > maxWriterQueueSize {
		queueSize = maxWriterQueueSize
	}
	writeTimeout := options.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = defaultWriterWriteTimeout
	}
	maxAttempts := options.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultWriterMaxAttempts
	} else if maxAttempts > maxWriterMaxAttempts {
		maxAttempts = maxWriterMaxAttempts
	}

	w := &Writer{
		sink:         sink,
		queue:        make(chan Span, queueSize),
		writeTimeout: writeTimeout,
		maxAttempts:  maxAttempts,
		done:         make(chan struct{}),
	}
	if sink == nil {
		w.closed = true
		close(w.done)
		return w
	}

	workerCtx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	go w.run(workerCtx)
	return w
}

// Submit validates and snapshots span before attempting a non-blocking enqueue.
func (w *Writer) Submit(span Span) bool {
	if w == nil {
		return false
	}
	if Validate(span) != nil {
		w.rejected.Add(1)
		return false
	}
	span = cloneSpan(span)

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		w.rejected.Add(1)
		return false
	}
	select {
	case w.queue <- span:
		w.accepted.Add(1)
		return true
	default:
		w.dropped.Add(1)
		return false
	}
}

func (w *Writer) Stats() WriterStats {
	if w == nil {
		return WriterStats{}
	}
	return WriterStats{
		Accepted:    w.accepted.Load(),
		Persisted:   w.persisted.Load(),
		Rejected:    w.rejected.Load(),
		Dropped:     w.dropped.Load(),
		WriteErrors: w.writeErrors.Load(),
	}
}

// Close prevents new submissions and waits for accepted spans to drain. If ctx
// expires first, Close cancels the worker; the sink's context compliance bounds
// how quickly that cancellation completes.
func (w *Writer) Close(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	done := w.done
	cancel := w.cancel
	w.mu.Unlock()

	select {
	case <-done:
		return nil
	default:
	}

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		if cancel != nil {
			cancel()
		}
		<-done
		return ctx.Err()
	}
}

func (w *Writer) run(ctx context.Context) {
	defer close(w.done)
	defer w.cancel()
	for {
		select {
		case <-ctx.Done():
			w.dropQueued()
			return
		case span, ok := <-w.queue:
			if !ok {
				return
			}
			if ctx.Err() != nil {
				w.dropped.Add(1)
				w.dropQueued()
				return
			}
			w.persist(ctx, span)
		}
	}
}

func (w *Writer) persist(ctx context.Context, span Span) {
	for attempt := 0; attempt < w.maxAttempts; attempt++ {
		writeCtx, cancel := context.WithTimeout(ctx, w.writeTimeout)
		err := w.sink.Write(writeCtx, span)
		cancel()
		if err == nil {
			w.persisted.Add(1)
			return
		}
		w.writeErrors.Add(1)
		if ctx.Err() != nil || attempt+1 == w.maxAttempts {
			w.dropped.Add(1)
			return
		}

		timer := time.NewTimer(writerRetryBackoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			w.dropped.Add(1)
			return
		}
	}
}

func (w *Writer) dropQueued() {
	for {
		select {
		case _, ok := <-w.queue:
			if !ok {
				return
			}
			w.dropped.Add(1)
		default:
			return
		}
	}
}

func cloneSpan(span Span) Span {
	span.EndedAt = clonePointer(span.EndedAt)
	span.DurationMS = clonePointer(span.DurationMS)
	span.Attributes.ChannelID = clonePointer(span.Attributes.ChannelID)
	span.Attributes.Attempt = clonePointer(span.Attributes.Attempt)
	span.Attributes.HTTPStatus = clonePointer(span.Attributes.HTTPStatus)
	span.Attributes.Width = clonePointer(span.Attributes.Width)
	span.Attributes.Height = clonePointer(span.Attributes.Height)
	span.Attributes.Seconds = clonePointer(span.Attributes.Seconds)
	span.Attributes.GenerateAudio = clonePointer(span.Attributes.GenerateAudio)
	return span
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
