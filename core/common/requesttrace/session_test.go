package requesttrace

import (
	"sync"
	"testing"
)

func TestSessionBuffersUntilOwnerIsBound(t *testing.T) {
	var saved []Span
	s := NewSession("same-client-request", func(v Span) bool { saved = append(saved, v); return true })
	auth := s.Begin(StageAuthentication, Attributes{})
	if len(saved) != 0 {
		t.Fatal("must not persist provisional ownership")
	}
	if !s.BindGroup("group-a") || s.BindGroup("group-b") {
		t.Fatal("owner must bind once")
	}
	auth.Finish(StatusSuccess)
	s.Finish(StatusSuccess)
	for _, v := range saved {
		if v.GroupID != "group-a" {
			t.Fatal("ownership changed")
		}
	}
	if len(saved) != 4 {
		t.Fatalf("got %d updates, want root and auth lifecycles", len(saved))
	}
}

func TestSessionRequestIDDoesNotDetermineTraceID(t *testing.T) {
	a := NewSession("same-client-request", func(Span) bool { return true })
	b := NewSession("same-client-request", func(Span) bool { return true })
	if a.TraceID() == "" || a.TraceID() == b.TraceID() {
		t.Fatalf("trace IDs must be fresh: %q %q", a.TraceID(), b.TraceID())
	}
}

func TestSessionUnauthenticatedFinishFlushesEmptyOwnership(t *testing.T) {
	var saved []Span
	s := NewSession("request", func(v Span) bool { saved = append(saved, v); return true })
	s.Begin(StageAuthentication, Attributes{}).Finish(StatusError)
	if !s.Finish(StatusError) {
		t.Fatal("root completion was not accepted")
	}
	for _, v := range saved {
		if v.GroupID != "" {
			t.Fatalf("unexpected owner %q", v.GroupID)
		}
	}
	if s.BindGroup("late-group") {
		t.Fatal("session rebound after anonymous finish")
	}
}

func TestSessionRejectsInvalidOwnerWithoutConsumingBinding(t *testing.T) {
	s := NewSession("request", func(Span) bool { return true })
	if s.BindGroup("") || s.BindGroup("bad\ngroup") {
		t.Fatal("invalid owner was accepted")
	}
	if !s.BindGroup("group-a") {
		t.Fatal("invalid owner consumed the binding")
	}
}

func TestRecorderAttributesAreDeepCopied(t *testing.T) {
	channel, attempt, status, width, height := 7, 2, 202, 512, 768
	seconds, audio := 1.5, true
	attrs := Attributes{ChannelID: &channel, Attempt: &attempt, HTTPStatus: &status, Width: &width, Height: &height, Seconds: &seconds, GenerateAudio: &audio}
	var saved []Span
	r := NewRecorder("request", func(v Span) bool { saved = append(saved, v); return true })
	h := r.BeginWithAttributes(StageUpstreamAttempt, "", attrs)
	channel, attempt, status, width, height, seconds, audio = 99, 99, 500, 1, 1, 9, false
	h.Finish(StatusSuccess)
	for _, span := range saved {
		if *span.Attributes.ChannelID != 7 || *span.Attributes.Attempt != 2 || *span.Attributes.HTTPStatus != 202 || *span.Attributes.Width != 512 || *span.Attributes.Height != 768 || *span.Attributes.Seconds != 1.5 || !*span.Attributes.GenerateAudio {
			t.Fatalf("attributes were aliased: %#v", span.Attributes)
		}
	}
}

func TestRecorderRejectedStartCanStillEmitCompletion(t *testing.T) {
	var saved []Span
	calls := 0
	r := NewRecorder("request", func(v Span) bool {
		calls++
		if calls == 1 {
			return false
		}
		saved = append(saved, v)
		return true
	})
	h := r.Begin(StageRequest, "")
	if !r.Truncated() {
		t.Fatal("rejected start did not truncate recorder")
	}
	if !h.Finish(StatusSuccess) || len(saved) != 1 || saved[0].Revision != 2 || !saved[0].Truncated {
		t.Fatalf("completion was disabled after rejected start: %#v", saved)
	}
}

func TestSessionConcurrentLifecycleAndAttempts(t *testing.T) {
	var emitMu sync.Mutex
	var saved []Span
	s := NewSession("request", func(v Span) bool { emitMu.Lock(); saved = append(saved, v); emitMu.Unlock(); return true })
	const workers = 100
	attempts := make(chan int, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			attempts <- s.NextAttempt()
			s.Begin(StageUpstreamAttempt, Attributes{}).Finish(StatusSuccess)
		}()
	}
	if !s.BindGroup("group-a") {
		t.Fatal("owner binding failed")
	}
	wg.Wait()
	if !s.Finish(StatusSuccess) {
		t.Fatal("root completion failed")
	}
	close(attempts)
	seen := make(map[int]bool)
	for n := range attempts {
		if n < 1 || n > workers || seen[n] {
			t.Fatalf("invalid or duplicate attempt %d", n)
		}
		seen[n] = true
	}
	emitMu.Lock()
	defer emitMu.Unlock()
	if len(saved) != 202 {
		t.Fatalf("got %d updates, want 202", len(saved))
	}
	rootSpanID := saved[0].SpanID
	rootCompletions := 0
	children := make(map[string]bool, workers)
	for _, span := range saved {
		if span.GroupID != "group-a" {
			t.Fatalf("mixed ownership: got %q", span.GroupID)
		}
		if span.SpanID == rootSpanID && span.Status != StatusRunning {
			rootCompletions++
		}
		if span.SpanID != rootSpanID {
			if span.ParentSpanID != rootSpanID {
				t.Fatalf("child %q parent = %q, want root %q", span.SpanID, span.ParentSpanID, rootSpanID)
			}
			children[span.SpanID] = true
		}
	}
	if rootCompletions != 1 || len(children) != workers {
		t.Fatalf("got %d root completions and %d children", rootCompletions, len(children))
	}
}

func TestSessionProvisionalBufferBoundary(t *testing.T) {
	var saved []Span
	s := NewSession("request", func(v Span) bool { saved = append(saved, v); return true })
	handles := make([]*Handle, 0, maxSpansPerRecorder-1)
	for range maxSpansPerRecorder - 1 {
		handles = append(handles, s.Begin(StageValidation, Attributes{}))
	}
	for _, h := range handles {
		h.Finish(StatusSuccess)
	}
	if len(saved) != 0 {
		t.Fatal("provisional events escaped")
	}
	if !s.BindGroup("group-a") {
		t.Fatal("valid owner was rejected")
	}
	if !s.Finish(StatusSuccess) {
		t.Fatal("512th lifecycle update was rejected")
	}
	if len(saved) != 2*maxSpansPerRecorder {
		t.Fatalf("got %d updates, want %d", len(saved), 2*maxSpansPerRecorder)
	}
}
