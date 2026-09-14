package model

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestTraceStoreCompletionBeforeStartKeepsTerminalRevision(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))

	completed := traceTestSpan(1)
	endedAt := completed.StartedAt.Add(1250 * time.Millisecond)
	duration := 1250.0
	completed.Status = requesttrace.StatusSuccess
	completed.EndedAt = &endedAt
	completed.DurationMS = &duration
	completed.Revision = 2

	require.NoError(t, store.Write(context.Background(), completed))

	started := traceTestSpan(1)
	require.NoError(t, store.Write(context.Background(), started))

	var spans []RequestTraceSpan
	require.NoError(t, db.Find(&spans).Error)
	require.Len(t, spans, 1)
	require.Equal(t, requesttrace.StatusSuccess, spans[0].Status)
	require.Equal(t, 2, spans[0].Revision)
	require.NotNil(t, spans[0].EndedAt)
	require.Equal(t, duration, *spans[0].DurationMS)

	var head RequestTraceHead
	require.NoError(t, db.First(&head).Error)
	require.Equal(t, 1, head.SpanCount)
}

func TestTraceStoreNewerCompletionUpdatesOnlyLifecycleFields(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))

	started := traceTestSpan(2)
	started.Attributes.PublicModelID = "original-model"
	require.NoError(t, store.Write(context.Background(), started))

	completed := started
	completed.Status = requesttrace.StatusError
	completed.Revision = 2
	endedAt := completed.StartedAt.Add(250 * time.Millisecond)
	duration := 250.0
	completed.EndedAt = &endedAt
	completed.DurationMS = &duration
	completed.Attributes.PublicModelID = "must-not-overwrite"
	require.NoError(t, store.Write(context.Background(), completed))

	var persisted RequestTraceSpan
	require.NoError(t, db.First(&persisted, "span_id = ?", started.SpanID).Error)
	require.Equal(t, requesttrace.StatusError, persisted.Status)
	require.Equal(t, 2, persisted.Revision)
	require.Equal(t, endedAt, *persisted.EndedAt)
	require.Equal(t, duration, *persisted.DurationMS)
	require.Equal(t, "original-model", persisted.Attributes.PublicModelID)
}

func TestTraceStoreRejectsImmutableIdentityChanges(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))

	original := traceTestSpan(10)
	require.NoError(t, store.Write(context.Background(), original))

	cases := map[string]func(*requesttrace.Span){
		"trace":   func(span *requesttrace.Span) { span.TraceID = traceTestID(900) },
		"service": func(span *requesttrace.Span) { span.Service = requesttrace.ServiceApp },
		"group":   func(span *requesttrace.Span) { span.GroupID = "another-group" },
		"request": func(span *requesttrace.Span) { span.RequestID = "another-request" },
		"parent":  func(span *requesttrace.Span) { span.ParentSpanID = traceTestID(901) },
		"stage":   func(span *requesttrace.Span) { span.Stage = requesttrace.StageResponse },
		"start":   func(span *requesttrace.Span) { span.StartedAt = span.StartedAt.Add(time.Second) },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			changed := original
			changed.Revision = 2
			mutate(&changed)
			require.Error(t, store.Write(context.Background(), changed))
		})
	}

	var persisted RequestTraceSpan
	require.NoError(t, db.First(&persisted, "span_id = ?", original.SpanID).Error)
	require.Equal(t, 1, persisted.Revision)
	require.Equal(t, original.TraceID, persisted.TraceID)
	require.Equal(t, original.RequestID, persisted.RequestID)

	var headCount int64
	require.NoError(t, db.Model(&RequestTraceHead{}).Count(&headCount).Error)
	require.Equal(t, int64(1), headCount)
}

func TestTraceStoreRejectsCrossGroupWriteWithoutConsumingQuota(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))

	first := traceTestSpan(20)
	require.NoError(t, store.Write(context.Background(), first))

	crossGroup := traceTestSpan(21)
	crossGroup.TraceID = first.TraceID
	crossGroup.GroupID = "another-group"
	require.Error(t, store.Write(context.Background(), crossGroup))

	var head RequestTraceHead
	require.NoError(t, db.First(&head).Error)
	require.Equal(t, 1, head.SpanCount)

	var spanCount int64
	require.NoError(t, db.Model(&RequestTraceSpan{}).Count(&spanCount).Error)
	require.Equal(t, int64(1), spanCount)
}

func TestTraceStoreRejectsSpanIDReuseAcrossTracesAndRollsBackHead(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))

	first := traceTestSpan(30)
	require.NoError(t, store.Write(context.Background(), first))

	otherTrace := traceTestSpan(31)
	otherTrace.SpanID = first.SpanID
	require.Error(t, store.Write(context.Background(), otherTrace))

	var heads []RequestTraceHead
	require.NoError(t, db.Find(&heads).Error)
	require.Len(t, heads, 1)
	require.Equal(t, first.TraceID, heads[0].TraceID)
	require.Equal(t, 1, heads[0].SpanCount)
}

func TestTraceStoreInsertFailureRollsBackReservedQuota(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))
	require.NoError(t, db.Exec(`
		CREATE TRIGGER reject_trace_span
		BEFORE INSERT ON request_trace_spans
		WHEN NEW.request_id = 'reject-insert'
		BEGIN
			SELECT RAISE(ABORT, 'forced trace insert failure');
		END
	`).Error)

	span := traceTestSpan(35)
	span.RequestID = "reject-insert"
	require.Error(t, store.Write(context.Background(), span))

	var headCount int64
	require.NoError(t, db.Model(&RequestTraceHead{}).Count(&headCount).Error)
	require.Zero(t, headCount)

	var spanCount int64
	require.NoError(t, db.Model(&RequestTraceSpan{}).Count(&spanCount).Error)
	require.Zero(t, spanCount)
}

func TestTraceStoreTerminalSpanCannotBeResurrectedOrRecompleted(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))

	unknown := traceTestSpan(40)
	unknown.Status = requesttrace.StatusUnknown
	unknown.Revision = 2
	require.NoError(t, store.Write(context.Background(), unknown))

	running := traceTestSpan(40)
	running.Revision = 3
	require.NoError(t, store.Write(context.Background(), running))

	failed := traceTestSpan(40)
	failed.Status = requesttrace.StatusError
	failed.Revision = 4
	endedAt := failed.StartedAt.Add(time.Second)
	duration := 1000.0
	failed.EndedAt = &endedAt
	failed.DurationMS = &duration
	require.NoError(t, store.Write(context.Background(), failed))

	var persisted RequestTraceSpan
	require.NoError(t, db.First(&persisted, "span_id = ?", unknown.SpanID).Error)
	require.Equal(t, requesttrace.StatusUnknown, persisted.Status)
	require.Equal(t, 2, persisted.Revision)
	require.Nil(t, persisted.EndedAt)
	require.Nil(t, persisted.DurationMS)
}

func TestTraceStoreIdempotentReplayDoesNotRefreshRetentionTimestamp(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))

	span := traceTestSpan(50)
	require.NoError(t, store.Write(context.Background(), span))

	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	require.NoError(t, db.Model(&RequestTraceSpan{}).
		Where("span_id = ?", span.SpanID).UpdateColumn("updated_at", old).Error)
	require.NoError(t, db.Model(&RequestTraceHead{}).
		Where("trace_id = ? AND service = ?", span.TraceID, span.Service).
		UpdateColumn("updated_at", old).Error)

	replay := span
	replay.Truncated = true
	require.NoError(t, store.Write(context.Background(), replay))

	var persisted RequestTraceSpan
	require.NoError(t, db.First(&persisted, "span_id = ?", span.SpanID).Error)
	require.True(t, persisted.UpdatedAt.Equal(old), "span replay refreshed updated_at: %v", persisted.UpdatedAt)

	var head RequestTraceHead
	require.NoError(t, db.First(&head).Error)
	require.True(t, head.UpdatedAt.Equal(old), "head replay refreshed updated_at: %v", head.UpdatedAt)
	require.True(t, head.Truncated)
}

func TestTraceStoreValidatesBeforeWritingAndHonorsCancelledContext(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))

	invalid := traceTestSpan(60)
	invalid.Attributes.PublicModelID = string(make([]byte, 2050))
	require.Error(t, store.Write(context.Background(), invalid))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, store.Write(ctx, traceTestSpan(61)), context.Canceled)

	var count int64
	require.NoError(t, db.Model(&RequestTraceHead{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestTraceStorePersistsSafeAttributeProjection(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))

	span := traceTestSpan(70)
	channelID := 12
	attempt := 0
	generateAudio := false
	span.Attributes = requesttrace.Attributes{
		PublicModelID: "public-model",
		ChannelID:     &channelID,
		Attempt:       &attempt,
		GenerateAudio: &generateAudio,
	}
	require.NoError(t, store.Write(context.Background(), span))

	var persisted RequestTraceSpan
	require.NoError(t, db.First(&persisted, "span_id = ?", span.SpanID).Error)
	require.Equal(t, span.Attributes, persisted.Attributes)
}

func openTraceSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "trace.db"))
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	return db
}

func traceTestSpan(sequence int) requesttrace.Span {
	return requesttrace.Span{
		Version:      requesttrace.Version,
		TraceID:      traceTestID(100000 + sequence),
		SpanID:       traceTestID(200000 + sequence),
		ParentSpanID: traceTestID(300000 + sequence),
		RequestID:    fmt.Sprintf("request-%d", sequence),
		GroupID:      "group-one",
		Service:      requesttrace.ServiceAIProxy,
		Stage:        requesttrace.StageRequest,
		Status:       requesttrace.StatusRunning,
		StartedAt:    time.Date(2026, 9, 8, 1, 2, sequence%60, 0, time.UTC),
		Revision:     1,
	}
}

func traceTestID(sequence int) string {
	return fmt.Sprintf("%032x", sequence)
}
