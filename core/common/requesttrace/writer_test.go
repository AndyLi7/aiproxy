package requesttrace

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWriterSubmitIsNonBlockingWhenQueueIsFull(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	w := NewWriter(sinkFunc(func(ctx context.Context, _ Span) error {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}), WriterOptions{QueueSize: 1, MaxAttempts: 1})

	if !w.Submit(validCompletedSpan()) {
		t.Fatal("first span was not accepted")
	}
	awaitSignal(t, started, "worker to enter sink")
	if !w.Submit(validCompletedSpan()) {
		t.Fatal("second span was not queued")
	}

	result := make(chan bool, 1)
	go func() { result <- w.Submit(validCompletedSpan()) }()
	select {
	case accepted := <-result:
		if accepted {
			t.Fatal("full queue accepted another span")
		}
	case <-time.After(time.Second):
		t.Fatal("Submit blocked on a full queue")
	}
	if got := w.Stats(); got.Accepted != 2 || got.Dropped != 1 || got.Rejected != 0 {
		t.Fatalf("unexpected full-queue stats: %+v", got)
	}

	close(release)
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWriterRetriesOnceThenPersists(t *testing.T) {
	var calls atomic.Int32
	w := NewWriter(sinkFunc(func(context.Context, Span) error {
		if calls.Add(1) == 1 {
			return errors.New("temporary")
		}
		return nil
	}), WriterOptions{})
	if !w.Submit(validCompletedSpan()) {
		t.Fatal("span was not accepted")
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("got %d writes, want 2", calls.Load())
	}
	if got := w.Stats(); got != (WriterStats{Accepted: 1, Persisted: 1, WriteErrors: 1}) {
		t.Fatalf("unexpected retry stats: %+v", got)
	}
}

func TestWriterDropsAfterBoundedAttempts(t *testing.T) {
	var calls atomic.Int32
	w := NewWriter(sinkFunc(func(context.Context, Span) error {
		calls.Add(1)
		return errors.New("permanent")
	}), WriterOptions{MaxAttempts: 99})
	if !w.Submit(validCompletedSpan()) {
		t.Fatal("span was not accepted")
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("got %d writes, want capped maximum of 2", calls.Load())
	}
	if got := w.Stats(); got != (WriterStats{Accepted: 1, Dropped: 1, WriteErrors: 2}) {
		t.Fatalf("unexpected exhausted-write stats: %+v", got)
	}
}

func TestWriterTimesOutSinkThroughContext(t *testing.T) {
	entered := make(chan struct{})
	w := NewWriter(sinkFunc(func(ctx context.Context, _ Span) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}), WriterOptions{WriteTimeout: 10 * time.Millisecond, MaxAttempts: 1})
	if !w.Submit(validCompletedSpan()) {
		t.Fatal("span was not accepted")
	}
	awaitSignal(t, entered, "sink call")
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.Stats(); got != (WriterStats{Accepted: 1, Dropped: 1, WriteErrors: 1}) {
		t.Fatalf("unexpected timeout stats: %+v", got)
	}
}

func TestWriterRejectsInvalidClosedAndDisabledSubmissions(t *testing.T) {
	w := NewWriter(sinkFunc(func(context.Context, Span) error { return nil }), WriterOptions{})
	invalid := validCompletedSpan()
	invalid.TraceID = "invalid"
	if w.Submit(invalid) {
		t.Fatal("invalid span was accepted")
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Submit(validCompletedSpan()) {
		t.Fatal("closed writer accepted a span")
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("repeated Close returned %v", err)
	}
	if got := w.Stats(); got != (WriterStats{Rejected: 2}) {
		t.Fatalf("unexpected rejected stats: %+v", got)
	}

	disabled := NewWriter(nil, WriterOptions{})
	if disabled.Submit(validCompletedSpan()) {
		t.Fatal("nil sink accepted a span")
	}
	if err := disabled.Close(context.Background()); err != nil {
		t.Fatalf("disabled writer Close returned %v", err)
	}
	if got := disabled.Stats(); got != (WriterStats{Rejected: 1}) {
		t.Fatalf("unexpected disabled stats: %+v", got)
	}
}

func TestWriterOptionsDefaultAndCapBounds(t *testing.T) {
	tests := []struct {
		name      string
		in        WriterOptions
		queueSize int
		timeout   time.Duration
		attempts  int
	}{
		{name: "zero defaults", in: WriterOptions{}, queueSize: 4096, timeout: time.Second, attempts: 2},
		{name: "negative defaults", in: WriterOptions{QueueSize: -1, WriteTimeout: -1, MaxAttempts: -1}, queueSize: 4096, timeout: time.Second, attempts: 2},
		{name: "upper caps", in: WriterOptions{QueueSize: 9999, WriteTimeout: 2 * time.Second, MaxAttempts: 99}, queueSize: 4096, timeout: 2 * time.Second, attempts: 2},
		{name: "positive values", in: WriterOptions{QueueSize: 3, WriteTimeout: 4 * time.Millisecond, MaxAttempts: 1}, queueSize: 3, timeout: 4 * time.Millisecond, attempts: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewWriter(sinkFunc(func(context.Context, Span) error { return nil }), tt.in)
			if cap(w.queue) != tt.queueSize || w.writeTimeout != tt.timeout || w.maxAttempts != tt.attempts {
				t.Fatalf("normalized options: queue=%d timeout=%s attempts=%d", cap(w.queue), w.writeTimeout, w.maxAttempts)
			}
			if err := w.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWriterSnapshotsEveryMutableSpanPointer(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	written := make(chan Span, 1)
	w := NewWriter(sinkFunc(func(ctx context.Context, span Span) error {
		close(entered)
		select {
		case <-release:
			written <- span
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}), WriterOptions{MaxAttempts: 1})

	span := validCompletedSpan()
	want := validCompletedSpan()
	if !w.Submit(span) {
		t.Fatal("span was not accepted")
	}
	awaitSignal(t, entered, "sink call")
	*span.EndedAt = span.EndedAt.Add(time.Hour)
	*span.DurationMS = 2
	*span.Attributes.ChannelID = 2
	*span.Attributes.Attempt = 2
	*span.Attributes.HTTPStatus = 201
	*span.Attributes.Width = 2
	*span.Attributes.Height = 2
	*span.Attributes.Seconds = 2
	*span.Attributes.GenerateAudio = true
	close(release)
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := <-written
	assertSpanPointersEqual(t, got, want)
}

func TestWriterCloseCancellationDropsInFlightAndQueuedSpans(t *testing.T) {
	entered := make(chan struct{})
	w := NewWriter(sinkFunc(func(ctx context.Context, _ Span) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}), WriterOptions{QueueSize: 1, WriteTimeout: time.Hour, MaxAttempts: 1})
	if !w.Submit(validCompletedSpan()) {
		t.Fatal("first span was not accepted")
	}
	awaitSignal(t, entered, "sink call")
	if !w.Submit(validCompletedSpan()) {
		t.Fatal("second span was not queued")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close returned %v, want context cancellation", err)
	}
	awaitSignal(t, w.done, "worker cancellation accounting")
	if got := w.Stats(); got != (WriterStats{Accepted: 2, Dropped: 2, WriteErrors: 1}) {
		t.Fatalf("unexpected canceled-close stats: %+v", got)
	}
}

func TestWriterCloseDeadlineReturnsBeforeCanceledSinkFinishesCleanup(t *testing.T) {
	entered := make(chan struct{})
	cancellationReceived := make(chan struct{})
	cleanupGate := make(chan struct{})
	var releaseOnce sync.Once
	releaseCleanup := func() { releaseOnce.Do(func() { close(cleanupGate) }) }
	defer releaseCleanup()
	w := NewWriter(sinkFunc(func(ctx context.Context, _ Span) error {
		close(entered)
		<-ctx.Done()
		close(cancellationReceived)
		<-cleanupGate
		return ctx.Err()
	}), WriterOptions{WriteTimeout: time.Hour, MaxAttempts: 1})
	if !w.Submit(validCompletedSpan()) {
		t.Fatal("span was not accepted")
	}
	awaitSignal(t, entered, "sink call")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	closeResult := make(chan error, 1)
	go func() { closeResult <- w.Close(ctx) }()
	awaitSignal(t, cancellationReceived, "sink cancellation")

	select {
	case err := <-closeResult:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close returned %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		releaseCleanup()
		<-closeResult
		t.Fatal("Close waited for sink cleanup after its context was canceled")
	}

	if got := w.Stats(); got != (WriterStats{Accepted: 1}) {
		t.Fatalf("stats were finalized before sink cleanup: %+v", got)
	}
	releaseCleanup()
	awaitSignal(t, w.done, "worker cleanup")
	if got := w.Stats(); got != (WriterStats{Accepted: 1, Dropped: 1, WriteErrors: 1}) {
		t.Fatalf("unexpected eventual canceled-close stats: %+v", got)
	}
}

func TestWriterConcurrentSubmitAndClose(t *testing.T) {
	w := NewWriter(sinkFunc(func(context.Context, Span) error { return nil }), WriterOptions{QueueSize: 256, MaxAttempts: 1})
	const submissions = 256
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range submissions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w.Submit(validCompletedSpan())
		}()
	}
	close(start)
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	stats := w.Stats()
	if stats.Accepted+stats.Rejected != submissions {
		t.Fatalf("accepted + rejected = %d, want %d: %+v", stats.Accepted+stats.Rejected, submissions, stats)
	}
	if stats.Persisted != stats.Accepted || stats.Dropped != 0 || stats.WriteErrors != 0 {
		t.Fatalf("unexpected concurrent stats: %+v", stats)
	}
}

func TestWriterQueuedWritesUseWorkerContext(t *testing.T) {
	caller, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	w := NewWriter(sinkFunc(func(ctx context.Context, _ Span) error {
		if caller.Err() == nil {
			return errors.New("test caller was not canceled")
		}
		return ctx.Err()
	}), WriterOptions{MaxAttempts: 1})
	if !w.Submit(validCompletedSpan()) {
		t.Fatal("span was not accepted after unrelated caller cancellation")
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.Stats(); got != (WriterStats{Accepted: 1, Persisted: 1}) {
		t.Fatalf("queued write inherited unrelated caller cancellation: %+v", got)
	}
}

type sinkFunc func(context.Context, Span) error

func (f sinkFunc) Write(ctx context.Context, span Span) error { return f(ctx, span) }

func awaitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func assertSpanPointersEqual(t *testing.T, got, want Span) {
	t.Helper()
	if got.EndedAt == nil || want.EndedAt == nil || !got.EndedAt.Equal(*want.EndedAt) {
		t.Fatalf("ended_at snapshot = %v, want %v", got.EndedAt, want.EndedAt)
	}
	if got.DurationMS == nil || *got.DurationMS != *want.DurationMS ||
		got.Attributes.ChannelID == nil || *got.Attributes.ChannelID != *want.Attributes.ChannelID ||
		got.Attributes.Attempt == nil || *got.Attributes.Attempt != *want.Attributes.Attempt ||
		got.Attributes.HTTPStatus == nil || *got.Attributes.HTTPStatus != *want.Attributes.HTTPStatus ||
		got.Attributes.Width == nil || *got.Attributes.Width != *want.Attributes.Width ||
		got.Attributes.Height == nil || *got.Attributes.Height != *want.Attributes.Height ||
		got.Attributes.Seconds == nil || *got.Attributes.Seconds != *want.Attributes.Seconds ||
		got.Attributes.GenerateAudio == nil || *got.Attributes.GenerateAudio != *want.Attributes.GenerateAudio {
		t.Fatalf("mutable pointer snapshot changed:\n got: %#v\nwant: %#v", got, want)
	}
}
