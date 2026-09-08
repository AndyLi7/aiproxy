package requesttrace

import "sync"

const maxBufferedSessionUpdates = maxSpansPerRecorder * 2

// Session owns the trace lifecycle until authentication binds an owner.
type Session struct {
	mu       sync.Mutex
	recorder *Recorder
	root     *Handle
	emit     func(Span) bool
	buffer   []Span
	bound    bool
	groupID  string
	attempt  int
}

// NewSession starts a provisional request root without exposing its ownership.
func NewSession(requestID string, emit func(Span) bool) *Session {
	s := &Session{emit: emit, buffer: make([]Span, 0, maxBufferedSessionUpdates)}
	s.recorder = NewRecorder(requestID, s.accept)
	s.root = s.recorder.Begin(StageRequest, "")
	return s
}

func (s *Session) accept(span Span) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	if !s.bound {
		if len(s.buffer) >= maxBufferedSessionUpdates {
			s.mu.Unlock()
			return false
		}
		s.buffer = append(s.buffer, span)
		s.mu.Unlock()
		return true
	}
	span.GroupID = s.groupID
	emit := s.emit
	s.mu.Unlock()
	return emit != nil && emit(span)
}

func (s *Session) TraceID() string {
	if s == nil || s.recorder == nil {
		return ""
	}
	return s.recorder.TraceID()
}

func (s *Session) RootID() string {
	if s == nil || s.root == nil {
		return ""
	}
	return s.root.SpanID()
}

// BindGroup fixes ownership once and flushes all provisional snapshots.
func (s *Session) BindGroup(groupID string) bool {
	if s == nil || validateRequiredString("group ID", groupID, maxGroupIDBytes) != nil {
		return false
	}
	return s.bind(groupID)
}

func (s *Session) bind(groupID string) bool {
	s.mu.Lock()
	if s.bound {
		s.mu.Unlock()
		return false
	}
	s.bound = true
	s.groupID = groupID
	buffered := s.buffer
	s.buffer = nil
	emit := s.emit
	s.mu.Unlock()

	for _, span := range buffered {
		span.GroupID = groupID
		if emit == nil || !emit(span) {
			s.recorder.markTruncated()
		}
	}
	return true
}

func (s *Session) Begin(stage Stage, attrs Attributes) *Handle {
	if s == nil || s.recorder == nil {
		return disabledHandle()
	}
	return s.recorder.BeginWithAttributes(stage, s.RootID(), attrs)
}

func (s *Session) NextAttempt() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempt++
	return s.attempt
}

func (s *Session) Finish(status Status) bool {
	if s == nil || s.root == nil || status == StatusRunning || !validStatus(status) {
		return false
	}
	s.mu.Lock()
	bound := s.bound
	s.mu.Unlock()
	if !bound {
		s.bind("")
	}
	return s.root.Finish(status)
}
