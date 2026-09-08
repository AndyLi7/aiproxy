package requesttrace

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFinishIsIdempotent(t *testing.T) {
	var saved []Span
	r := NewRecorder("request-example", func(s Span) bool {
		saved = append(saved, s)
		return true
	})
	h := r.Begin(StageRequest, "")
	if !h.Finish(StatusSuccess) || h.Finish(StatusError) {
		t.Fatal("finish must occur exactly once")
	}
	if len(saved) != 2 || saved[1].Status != StatusSuccess {
		t.Fatalf("expected start and one completion: %#v", saved)
	}
	if saved[1].EndedAt == nil || saved[1].DurationMS == nil || *saved[1].DurationMS < 0 {
		t.Fatalf("expected a non-negative measured completion: %#v", saved[1])
	}
}

func TestRecorderGeneratesIndependentRandomTraceIDs(t *testing.T) {
	r1 := NewRecorder("same-external-request", func(Span) bool { return true })
	r2 := NewRecorder("same-external-request", func(Span) bool { return true })

	if r1.TraceID() == r2.TraceID() {
		t.Fatalf("duplicate external request IDs reused trace ID %q", r1.TraceID())
	}
	idPattern := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for _, traceID := range []string{r1.TraceID(), r2.TraceID()} {
		if !idPattern.MatchString(traceID) {
			t.Fatalf("trace ID is not 32 lowercase hexadecimal characters: %q", traceID)
		}
	}
}

func TestRecorderEmitsValidLifecycle(t *testing.T) {
	var saved []Span
	r := NewRecorder("request-example", func(s Span) bool {
		saved = append(saved, s)
		return true
	})
	h := r.Begin(StageRequest, "")
	if !h.Finish(StatusTimeout) {
		t.Fatal("expected completion to be accepted")
	}

	if len(saved) != 2 {
		t.Fatalf("got %d lifecycle events, want 2", len(saved))
	}
	for i, span := range saved {
		if err := Validate(span); err != nil {
			t.Fatalf("event %d is invalid: %v (%#v)", i, err, span)
		}
	}
	if saved[0].Version != Version || saved[0].Service != ServiceAIProxy || saved[0].Revision != 1 {
		t.Fatalf("unexpected start contract: %#v", saved[0])
	}
	if saved[0].Status != StatusRunning || saved[0].EndedAt != nil || saved[0].DurationMS != nil {
		t.Fatalf("start must be running without completion fields: %#v", saved[0])
	}
	if saved[1].Revision != 2 || saved[1].SpanID != saved[0].SpanID || saved[1].StartedAt != saved[0].StartedAt {
		t.Fatalf("completion must update the original span: start=%#v end=%#v", saved[0], saved[1])
	}
}

func TestRecorderOnlyAcceptsParentsItGenerated(t *testing.T) {
	r := NewRecorder("request-example", func(Span) bool { return true })
	external := r.Begin(StageValidation, strings.Repeat("d", 32))
	if external.SpanID() != "" || external.Finish(StatusSuccess) {
		t.Fatal("random-looking external parent ID was accepted")
	}
	parent := r.Begin(StageRequest, "")
	child := r.Begin(StageValidation, parent.SpanID())
	if child.SpanID() == "" || !child.Finish(StatusSuccess) {
		t.Fatal("recorder rejected its own span as a parent")
	}
}

func TestFinishReportsEmitterRejectionOnce(t *testing.T) {
	emissions := 0
	r := NewRecorder("request-example", func(Span) bool {
		emissions++
		return emissions == 1
	})
	h := r.Begin(StageRequest, "")
	if h.Finish(StatusSuccess) || h.Finish(StatusError) {
		t.Fatal("rejected completion must remain finished and report false")
	}
	if emissions != 2 {
		t.Fatalf("got %d emissions, want one start and one completion attempt", emissions)
	}
	if !r.Truncated() {
		t.Fatal("rejected completion did not truncate recorder")
	}
}

func TestInvalidFinishDoesNotConsumeTheValidFinish(t *testing.T) {
	r := NewRecorder("request-example", func(Span) bool { return true })
	h := r.Begin(StageRequest, "")
	if h.Finish(StatusRunning) || h.Finish(Status("invented")) {
		t.Fatal("invalid finish status was accepted")
	}
	if !h.Finish(StatusSuccess) {
		t.Fatal("invalid status consumed the one valid finish")
	}
}

func TestInvalidRecorderInputsRemainNilSafe(t *testing.T) {
	for _, recorder := range []*Recorder{
		NewRecorder(strings.Repeat("r", 129), func(Span) bool { return true }),
		NewRecorder("request-example", nil),
		nil,
	} {
		h := recorder.Begin(StageRequest, "")
		if h == nil || h.SpanID() != "" || h.Finish(StatusSuccess) {
			t.Fatal("invalid recorder input did not produce a safe disabled handle")
		}
	}
}

func TestRecorderEntropyFailureReturnsSafeDisabledHandle(t *testing.T) {
	original := rand.Reader
	rand.Reader = failingReader{}
	t.Cleanup(func() { rand.Reader = original })

	emitted := 0
	r := NewRecorder("request-example", func(Span) bool {
		emitted++
		return true
	})
	h := r.Begin(StageRequest, "")
	if h == nil {
		t.Fatal("Begin returned nil instead of a disabled handle")
	}
	if h.Finish(StatusSuccess) {
		t.Fatal("disabled handle reported a completion")
	}
	if r.TraceID() != "" || emitted != 0 {
		t.Fatalf("entropy failure leaked fallback identity or event: trace=%q emitted=%d", r.TraceID(), emitted)
	}
}

func TestRecorderSpanEntropyFailureDisablesRecorder(t *testing.T) {
	var calls int
	original := rand.Reader
	rand.Reader = readerFunc(func(p []byte) (int, error) {
		calls++
		if calls == 1 {
			for i := range p {
				p[i] = byte(i + 1)
			}
			return len(p), nil
		}
		return 0, errors.New("entropy unavailable")
	})
	t.Cleanup(func() { rand.Reader = original })

	r := NewRecorder("request-example", func(Span) bool { return true })
	first := r.Begin(StageRequest, "")
	second := r.Begin(StageRequest, "")
	if first == nil || first.Finish(StatusSuccess) || second == nil || second.Finish(StatusSuccess) {
		t.Fatal("span entropy failure must return disabled handles and disable the recorder")
	}
}

func TestRecorderRejectsMoreThan256SpansButAllowsExistingCompletion(t *testing.T) {
	var mu sync.Mutex
	var saved []Span
	r := NewRecorder("request-example", func(s Span) bool {
		mu.Lock()
		saved = append(saved, s)
		mu.Unlock()
		return true
	})

	handles := make([]*Handle, 0, maxSpansPerRecorder)
	for i := 0; i < maxSpansPerRecorder; i++ {
		handles = append(handles, r.Begin(StageUpstreamAttempt, ""))
	}
	rejected := r.Begin(StageUpstreamAttempt, "")
	if rejected == nil || rejected.Finish(StatusSuccess) {
		t.Fatal("over-limit Begin must return a safe disabled handle")
	}
	if !r.Truncated() {
		t.Fatal("recorder did not retain truncated state")
	}
	if !handles[0].Finish(StatusSuccess) {
		t.Fatal("existing span could not complete after the limit was reached")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(saved) != maxSpansPerRecorder+1 {
		t.Fatalf("got %d events, want 256 starts and one existing completion", len(saved))
	}
	if !saved[len(saved)-1].Truncated {
		t.Fatal("completion after overflow did not propagate truncated state")
	}
}

func TestRecorderConcurrentBeginAndFinish(t *testing.T) {
	var mu sync.Mutex
	seen := make(map[string]int)
	r := NewRecorder("request-example", func(s Span) bool {
		mu.Lock()
		seen[s.SpanID]++
		mu.Unlock()
		return true
	})

	var wg sync.WaitGroup
	for range maxSpansPerRecorder {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := r.Begin(StageChannelSelection, "")
			if !h.Finish(StatusSuccess) {
				t.Errorf("accepted handle failed to finish")
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != maxSpansPerRecorder {
		t.Fatalf("got %d unique spans, want %d", len(seen), maxSpansPerRecorder)
	}
	for id, events := range seen {
		if events != 2 {
			t.Fatalf("span %s emitted %d events, want 2", id, events)
		}
	}
}

func TestValidateRejectsUnsafeContractValues(t *testing.T) {
	valid := validCompletedSpan()
	nan := math.NaN()
	inf := math.Inf(1)
	negative := -1
	badHTTPStatus := 99
	negativeChannelID := -1

	tests := []struct {
		name   string
		mutate func(*Span)
	}{
		{"unsupported version", func(s *Span) { s.Version = 2 }},
		{"uppercase trace ID", func(s *Span) { s.TraceID = strings.Repeat("A", 32) }},
		{"external-looking span ID", func(s *Span) { s.SpanID = "customer-value" }},
		{"bad parent ID", func(s *Span) { s.ParentSpanID = "parent" }},
		{"empty request ID", func(s *Span) { s.RequestID = "" }},
		{"long request ID", func(s *Span) { s.RequestID = strings.Repeat("r", 129) }},
		{"request control character", func(s *Span) { s.RequestID = "request\nsecret" }},
		{"long group ID", func(s *Span) { s.GroupID = strings.Repeat("g", 65) }},
		{"invalid service", func(s *Span) { s.Service = Service("gateway") }},
		{"invalid stage", func(s *Span) { s.Stage = Stage("database_query") }},
		{"invalid status", func(s *Span) { s.Status = Status("finished") }},
		{"zero revision", func(s *Span) { s.Revision = 0 }},
		{"running with end", func(s *Span) { s.Status = StatusRunning }},
		{"completion without end", func(s *Span) { s.EndedAt = nil }},
		{"completion without duration", func(s *Span) { s.DurationMS = nil }},
		{"negative duration", func(s *Span) { s.DurationMS = floatPtr(-0.1) }},
		{"NaN duration", func(s *Span) { s.DurationMS = &nan }},
		{"infinite duration", func(s *Span) { s.DurationMS = &inf }},
		{"long public model", func(s *Span) { s.Attributes.PublicModelID = strings.Repeat("m", 129) }},
		{"attribute control character", func(s *Span) { s.Attributes.UpstreamModelID = "model\tsecret" }},
		{"negative channel ID", func(s *Span) { s.Attributes.ChannelID = &negativeChannelID }},
		{"bad error code", func(s *Span) { s.Attributes.ErrorCode = "UPSTREAM-FAIL" }},
		{"negative attempt", func(s *Span) { s.Attributes.Attempt = &negative }},
		{"bad HTTP status", func(s *Span) { s.Attributes.HTTPStatus = &badHTTPStatus }},
		{"NaN seconds", func(s *Span) { s.Attributes.Seconds = &nan }},
		{"infinite seconds", func(s *Span) { s.Attributes.Seconds = &inf }},
		{"invalid aspect ratio", func(s *Span) { s.Attributes.AspectRatio = "16/9" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			span := valid
			tt.mutate(&span)
			if err := Validate(span); err == nil {
				t.Fatalf("Validate accepted unsafe span: %#v", span)
			}
		})
	}
}

func TestValidateRejectsAttributesOver2048BytesAtTheAggregateBoundary(t *testing.T) {
	span := validCompletedSpan()
	span.Attributes.PublicModelID = strings.Repeat("x", maxAttributesBytes+1)
	if err := Validate(span); !errors.Is(err, errAttributesTooLarge) {
		t.Fatalf("got %v, want aggregate attribute size rejection", err)
	}
}

func TestValidateUnknownStatusKeepsUnobservedCompletionTimesEmpty(t *testing.T) {
	span := validCompletedSpan()
	span.Status = StatusUnknown
	span.EndedAt = nil
	span.DurationMS = nil
	if err := Validate(span); err != nil {
		t.Fatalf("unknown unobserved span rejected: %v", err)
	}
	span.EndedAt = timePtr(time.Now().UTC())
	if err := Validate(span); err == nil {
		t.Fatal("unknown span accepted a fabricated end time")
	}
}

func TestJSONRejectsUnknownFields(t *testing.T) {
	validJSON, err := json.Marshal(validCompletedSpan())
	if err != nil {
		t.Fatal(err)
	}

	tests := []string{
		strings.TrimSuffix(string(validJSON), "}") + `,"prompt":"secret"}`,
		strings.Replace(string(validJSON), `"attributes":{`, `"attributes":{"response_body":"secret",`, 1),
	}
	for _, input := range tests {
		var span Span
		if err := json.Unmarshal([]byte(input), &span); err == nil {
			t.Fatalf("accepted unknown JSON field: %s", input)
		}
	}
}

func validCompletedSpan() Span {
	started := time.Date(2026, 9, 8, 1, 2, 3, 0, time.UTC)
	ended := started.Add(time.Second)
	duration := 1000.0
	attempt := 0
	status := 200
	channelID := 1
	width := 0
	height := 1024
	seconds := 0.0
	audio := false
	return Span{
		Version:      Version,
		TraceID:      strings.Repeat("a", 32),
		SpanID:       strings.Repeat("b", 32),
		ParentSpanID: strings.Repeat("c", 32),
		RequestID:    "request-example",
		GroupID:      "group-example",
		Service:      ServiceAIProxy,
		Stage:        StageUpstreamAttempt,
		Status:       StatusSuccess,
		StartedAt:    started,
		EndedAt:      &ended,
		DurationMS:   &duration,
		Revision:     2,
		Attributes: Attributes{
			PublicModelID:   "public-model",
			UpstreamModelID: "upstream-model",
			ChannelID:       &channelID,
			Attempt:         &attempt,
			ErrorCode:       "upstream_timeout",
			HTTPStatus:      &status,
			Width:           &width,
			Height:          &height,
			Seconds:         &seconds,
			AspectRatio:     "16:9",
			GenerateAudio:   &audio,
		},
	}
}

func floatPtr(value float64) *float64 { return &value }

func timePtr(value time.Time) *time.Time { return &value }

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
