package requesttrace

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"sync"
	"time"
)

type Recorder struct {
	mu        sync.RWMutex
	traceID   string
	requestID string
	emit      func(Span) bool
	spanIDs   map[string]struct{}
	spanCount int
	disabled  bool
	truncated bool
}

// NewRecorder creates an aiproxy recorder with a fresh system trace ID.
// Invalid input, a nil emitter, or unavailable entropy creates a disabled
// recorder whose Begin method remains safe to call.
func NewRecorder(requestID string, emit func(Span) bool) *Recorder {
	recorder := &Recorder{
		requestID: requestID,
		emit:      emit,
		spanIDs:   make(map[string]struct{}),
	}
	if validateRequiredString("request ID", requestID, maxRequestIDBytes) != nil || emit == nil {
		recorder.disabled = true
		return recorder
	}
	traceID, err := newRandomID()
	if err != nil {
		recorder.disabled = true
		return recorder
	}
	recorder.traceID = traceID
	return recorder
}

func (r *Recorder) TraceID() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.traceID
}

func (r *Recorder) Truncated() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.truncated
}

func (r *Recorder) markTruncated() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.truncated = true
	r.mu.Unlock()
}

// Begin emits a running span. The returned handle is always non-nil.
func (r *Recorder) Begin(stage Stage, parentID string) *Handle {
	return r.BeginWithAttributes(stage, parentID, Attributes{})
}

// BeginWithAttributes emits a running span with an immutable snapshot of the
// supplied whitelist attributes. The returned handle is always non-nil.
func (r *Recorder) BeginWithAttributes(stage Stage, parentID string, attrs Attributes) *Handle {
	if r == nil {
		return disabledHandle()
	}
	started := time.Now()
	attrs = cloneAttributes(attrs)
	r.mu.Lock()
	if r.disabled || !validStage(stage) || validateAttributes(attrs) != nil || (parentID != "" && !r.ownsSpan(parentID)) {
		r.mu.Unlock()
		return disabledHandle()
	}
	if r.spanCount >= maxSpansPerRecorder {
		r.truncated = true
		r.mu.Unlock()
		return disabledHandle()
	}
	spanID, err := newRandomID()
	if err != nil {
		r.disabled = true
		r.mu.Unlock()
		return disabledHandle()
	}
	r.spanCount++
	r.spanIDs[spanID] = struct{}{}
	span := Span{
		Version:      Version,
		TraceID:      r.traceID,
		SpanID:       spanID,
		ParentSpanID: parentID,
		RequestID:    r.requestID,
		Service:      ServiceAIProxy,
		Stage:        stage,
		Status:       StatusRunning,
		StartedAt:    started.UTC(),
		Revision:     1,
		Attributes:   attrs,
	}
	emit := r.emit
	r.mu.Unlock()

	if !emit(span) {
		r.markTruncated()
	}
	return &Handle{recorder: r, span: span, started: started}
}

func cloneAttributes(attrs Attributes) Attributes {
	cloned := attrs
	if attrs.ChannelID != nil {
		value := *attrs.ChannelID
		cloned.ChannelID = &value
	}
	if attrs.Attempt != nil {
		value := *attrs.Attempt
		cloned.Attempt = &value
	}
	if attrs.HTTPStatus != nil {
		value := *attrs.HTTPStatus
		cloned.HTTPStatus = &value
	}
	if attrs.Width != nil {
		value := *attrs.Width
		cloned.Width = &value
	}
	if attrs.Height != nil {
		value := *attrs.Height
		cloned.Height = &value
	}
	if attrs.Seconds != nil {
		value := *attrs.Seconds
		cloned.Seconds = &value
	}
	if attrs.GenerateAudio != nil {
		value := *attrs.GenerateAudio
		cloned.GenerateAudio = &value
	}
	return cloned
}

func (r *Recorder) ownsSpan(spanID string) bool {
	_, ok := r.spanIDs[spanID]
	return ok
}

func (r *Recorder) currentTruncated() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.truncated
}

type Handle struct {
	recorder *Recorder
	span     Span
	started  time.Time
	once     sync.Once
}

func disabledHandle() *Handle { return &Handle{} }

func (h *Handle) SpanID() string {
	if h == nil {
		return ""
	}
	return h.span.SpanID
}

// Finish emits at most one final snapshot. Its result reports whether this
// call won the finish race and the emitter accepted that snapshot.
func (h *Handle) Finish(status Status) bool {
	if h == nil || h.recorder == nil || status == StatusRunning || !validStatus(status) {
		return false
	}
	accepted := false
	h.once.Do(func() {
		finished := h.span
		finished.Status = status
		finished.Revision = 2
		finished.Truncated = h.recorder.currentTruncated()
		if status != StatusUnknown {
			ended := time.Now()
			endedUTC := ended.UTC()
			duration := float64(time.Since(h.started).Microseconds()) / 1000
			finished.EndedAt = &endedUTC
			finished.DurationMS = &duration
		}
		if Validate(finished) != nil {
			return
		}
		accepted = h.recorder.emit(finished)
		if !accepted {
			h.recorder.markTruncated()
		}
	})
	return accepted
}

func newRandomID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}
