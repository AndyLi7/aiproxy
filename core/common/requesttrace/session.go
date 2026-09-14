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
	trusted  VerifiedContext
}

// NewSession starts a provisional request root without exposing its ownership.
func NewSession(requestID string, emit func(Span) bool) *Session {
	return newSession(requestID, emit, StageRequest)
}

// NewTaskPollSession represents a background observation, not an HTTP request.
func NewTaskPollSession(requestID string, emit func(Span) bool) *Session {
	return newSession(requestID, emit, StageAsyncStatusPoll)
}

func newSession(requestID string, emit func(Span) bool, stage Stage) *Session {
	s := &Session{emit: emit, buffer: make([]Span, 0, maxBufferedSessionUpdates)}
	s.recorder = NewRecorder(requestID, s.accept)
	s.root = s.recorder.Begin(stage, "")
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
	span = s.projectLocked(span)
	emit := s.emit
	s.mu.Unlock()
	return emit != nil && emit(span)
}

func (s *Session) TraceID() string {
	if s == nil || s.recorder == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.trusted.TraceID() != "" {
		return s.trusted.TraceID()
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
	return s.bindContext(groupID, VerifiedContext{})
}

// BindVerifiedContext accepts only a previously verified context for this request.
// Projection happens before persistence; recorder-local parent validation stays intact.
func (s *Session) BindVerifiedContext(groupID string, verified VerifiedContext) bool {
	if s == nil || verified.claims.GroupID != groupID || groupID == "" || verified.claims.RequestID != s.recorder.requestID || !validRandomID(verified.TraceID()) || !validRandomID(verified.ParentSpanID()) {
		return false
	}
	return s.bindContext(groupID, verified)
}

// BindStoredTask is for a server-resolved, owner-checked persisted association.
// Never call this with header or URL values as trace identifiers.
func (s *Session) BindStoredTask(groupID, traceID, parentID string) bool {
	if s == nil || s.recorder == nil || validateRequiredString("group", groupID, maxGroupIDBytes) != nil {
		return false
	}
	return s.BindVerifiedContext(groupID, VerifiedContext{claims: TrustedClaims{GroupID: groupID, RequestID: s.recorder.requestID, TraceID: traceID, ParentSpanID: parentID}})
}

func (s *Session) projectLocked(span Span) Span {
	span.GroupID = s.groupID
	if s.trusted.TraceID() != "" {
		span.TraceID = s.trusted.TraceID()
		if span.SpanID == s.RootID() {
			span.ParentSpanID = s.trusted.ParentSpanID()
		}
	}
	return span
}

func (s *Session) bindContext(groupID string, verified VerifiedContext) bool {
	s.mu.Lock()
	if s.bound {
		s.mu.Unlock()
		return false
	}
	s.bound = true
	s.groupID = groupID
	s.trusted = verified
	buffered := s.buffer
	for i := range buffered {
		buffered[i] = s.projectLocked(buffered[i])
	}
	s.buffer = nil
	emit := s.emit
	s.mu.Unlock()

	for _, span := range buffered {
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
