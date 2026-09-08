package model

import (
	"context"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/stretchr/testify/require"
)

func TestTraceStoreListScopesByGroupTraceAndServiceAndPaginatesBySpanID(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))

	traceID := traceTestID(600)
	for _, fixture := range []struct {
		sequence int
		group    string
		service  requesttrace.Service
		traceID  string
	}{
		{601, "group-a", requesttrace.ServiceAIProxy, traceID},
		{602, "group-a", requesttrace.ServiceAIProxy, traceID},
		{603, "group-a", requesttrace.ServiceAIProxy, traceID},
		{604, "group-b", requesttrace.ServiceAIProxy, traceTestID(604)},
		{605, "group-a", requesttrace.ServiceApp, traceID},
		{606, "", requesttrace.ServiceAIProxy, traceTestID(606)},
	} {
		span := traceTestSpan(fixture.sequence)
		span.TraceID = fixture.traceID
		span.GroupID = fixture.group
		span.Service = fixture.service
		if fixture.sequence == 601 || fixture.sequence == 604 {
			span.RequestID = "same-request-id"
		}
		require.NoError(t, store.Write(context.Background(), span))
	}
	require.NoError(t, db.Create(&RequestTraceSpan{
		SpanID:    traceTestID(200607),
		Version:   requesttrace.Version,
		TraceID:   traceTestID(606),
		GroupID:   "group-a",
		Service:   requesttrace.ServiceAIProxy,
		RequestID: "same-request-id",
		Stage:     requesttrace.StageRequest,
		Status:    requesttrace.StatusRunning,
		StartedAt: time.Date(2026, 9, 8, 1, 2, 7, 0, time.UTC),
		Revision:  1,
	}).Error)

	first, err := store.List(context.Background(), TraceQuery{
		GroupID: "group-a", TraceID: traceID, Service: requesttrace.ServiceAIProxy, Limit: 2,
	})
	require.NoError(t, err)
	require.Equal(t, []string{traceTestID(200601), traceTestID(200602)}, tracePageSpanIDs(first))
	require.False(t, first.Truncated)
	require.Equal(t, traceTestID(200602), first.NextCursor)

	second, err := store.List(context.Background(), TraceQuery{
		GroupID: "group-a", TraceID: traceID, Service: requesttrace.ServiceAIProxy,
		AfterSpanID: first.NextCursor, Limit: 2,
	})
	require.NoError(t, err)
	require.Equal(t, []string{traceTestID(200603)}, tracePageSpanIDs(second))
	require.False(t, second.Truncated)
	require.Empty(t, second.NextCursor)

	emptyGroup, err := store.List(context.Background(), TraceQuery{
		TraceID: traceTestID(606), Service: requesttrace.ServiceAIProxy,
	})
	require.NoError(t, err)
	require.Equal(t, []string{traceTestID(200606)}, tracePageSpanIDs(emptyGroup))
}

func TestTraceStoreListUsesDefaultAndMaximumLimitsAndReturnsDatabaseErrors(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))

	traceID := traceTestID(700)
	for i := 0; i < 101; i++ {
		span := traceTestSpan(700 + i)
		span.TraceID = traceID
		require.NoError(t, store.Write(context.Background(), span))
	}

	page, err := store.List(context.Background(), TraceQuery{
		GroupID: "group-one", TraceID: traceID, Service: requesttrace.ServiceAIProxy,
	})
	require.NoError(t, err)
	require.Len(t, page.Items, 50)
	require.False(t, page.Truncated)

	page, err = store.List(context.Background(), TraceQuery{
		GroupID: "group-one", TraceID: traceID, Service: requesttrace.ServiceAIProxy, Limit: 1000,
	})
	require.NoError(t, err)
	require.Len(t, page.Items, 100)
	require.False(t, page.Truncated)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = store.List(ctx, TraceQuery{GroupID: "group-one", TraceID: traceID, Service: requesttrace.ServiceAIProxy})
	require.ErrorIs(t, err, context.Canceled)

	_, err = NewTraceStore(nil).List(context.Background(), TraceQuery{})
	require.Error(t, err)
}

func TestTraceStoreListUsesPersistedTraceTruncationNotPaginationLookahead(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))

	truncatedTraceID := traceTestID(750)
	for sequence := 750; sequence < 753; sequence++ {
		span := traceTestSpan(sequence)
		span.TraceID = truncatedTraceID
		span.Truncated = sequence == 752
		require.NoError(t, store.Write(context.Background(), span))
	}

	first, err := store.List(context.Background(), TraceQuery{
		GroupID: "group-one", TraceID: truncatedTraceID, Service: requesttrace.ServiceAIProxy, Limit: 2,
	})
	require.NoError(t, err)
	require.True(t, first.Truncated)
	require.Equal(t, traceTestID(200751), first.NextCursor)
	for _, item := range first.Items {
		require.True(t, item.Truncated)
	}

	last, err := store.List(context.Background(), TraceQuery{
		GroupID: "group-one", TraceID: truncatedTraceID, Service: requesttrace.ServiceAIProxy,
		AfterSpanID: first.NextCursor, Limit: 2,
	})
	require.NoError(t, err)
	require.True(t, last.Truncated, "persisted truncation remains visible after the final page")
	require.Empty(t, last.NextCursor)
	require.Len(t, last.Items, 1)
	require.True(t, last.Items[0].Truncated)

	emptyTraceID := traceTestID(755)
	require.NoError(t, db.Create(&RequestTraceHead{
		TraceID: emptyTraceID, Service: requesttrace.ServiceAIProxy, GroupID: "group-one", Truncated: true,
	}).Error)
	empty, err := store.List(context.Background(), TraceQuery{
		GroupID: "group-one", TraceID: emptyTraceID, Service: requesttrace.ServiceAIProxy,
	})
	require.NoError(t, err)
	require.Empty(t, empty.Items)
	require.True(t, empty.Truncated)
	require.Empty(t, empty.NextCursor)

	normalTraceID := traceTestID(760)
	for sequence := 760; sequence < 763; sequence++ {
		span := traceTestSpan(sequence)
		span.TraceID = normalTraceID
		require.NoError(t, store.Write(context.Background(), span))
	}
	normal, err := store.List(context.Background(), TraceQuery{
		GroupID: "group-one", TraceID: normalTraceID, Service: requesttrace.ServiceAIProxy, Limit: 2,
	})
	require.NoError(t, err)
	require.False(t, normal.Truncated, "a next cursor is pagination, not trace-data truncation")
	require.Equal(t, traceTestID(200761), normal.NextCursor)
	for _, item := range normal.Items {
		require.False(t, item.Truncated)
	}

	crossGroup := traceTestSpan(770)
	crossGroup.TraceID = truncatedTraceID
	crossGroup.GroupID = "group-other"
	require.NoError(t, db.Create(&RequestTraceSpan{
		SpanID:       crossGroup.SpanID,
		Version:      crossGroup.Version,
		TraceID:      crossGroup.TraceID,
		GroupID:      crossGroup.GroupID,
		Service:      crossGroup.Service,
		RequestID:    crossGroup.RequestID,
		ParentSpanID: crossGroup.ParentSpanID,
		Stage:        crossGroup.Stage,
		Status:       crossGroup.Status,
		StartedAt:    crossGroup.StartedAt,
		Revision:     crossGroup.Revision,
		Attributes:   crossGroup.Attributes,
	}).Error)
	crossGroupPage, err := store.List(context.Background(), TraceQuery{
		GroupID: "group-other", TraceID: truncatedTraceID, Service: requesttrace.ServiceAIProxy,
	})
	require.NoError(t, err)
	require.False(t, crossGroupPage.Truncated)
	require.Len(t, crossGroupPage.Items, 1)
	require.False(t, crossGroupPage.Items[0].Truncated)
}

func TestTraceStoreFindRequestsScopesExactRootRequestsAndPaginatesCandidates(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))

	now := time.Now().UTC()
	fixtures := []RequestTraceSpan{
		{SpanID: traceTestID(901), TraceID: traceTestID(801), GroupID: "group-a", RequestID: "reused-request", Service: requesttrace.ServiceAIProxy, Stage: requesttrace.StageRequest, Status: requesttrace.StatusSuccess, StartedAt: now.Add(-time.Minute), UpdatedAt: now},
		{SpanID: traceTestID(902), TraceID: traceTestID(802), GroupID: "group-a", RequestID: "reused-request", Service: requesttrace.ServiceAIProxy, Stage: requesttrace.StageRequest, Status: requesttrace.StatusError, StartedAt: now, UpdatedAt: now},
		{SpanID: traceTestID(903), TraceID: traceTestID(803), GroupID: "group-b", RequestID: "reused-request", Service: requesttrace.ServiceAIProxy, Stage: requesttrace.StageRequest, Status: requesttrace.StatusSuccess, StartedAt: now, UpdatedAt: now},
		{SpanID: traceTestID(904), TraceID: traceTestID(804), GroupID: "group-a", RequestID: "reused-request", Service: requesttrace.ServiceApp, Stage: requesttrace.StageRequest, Status: requesttrace.StatusSuccess, StartedAt: now, UpdatedAt: now},
		{SpanID: traceTestID(905), TraceID: traceTestID(805), GroupID: "group-a", RequestID: "reused-request", Service: requesttrace.ServiceAIProxy, Stage: requesttrace.StageGatewayCall, Status: requesttrace.StatusSuccess, StartedAt: now, UpdatedAt: now},
		{SpanID: traceTestID(906), TraceID: traceTestID(806), GroupID: "group-a", RequestID: "reused-request", ParentSpanID: traceTestID(1), Service: requesttrace.ServiceAIProxy, Stage: requesttrace.StageRequest, Status: requesttrace.StatusSuccess, StartedAt: now, UpdatedAt: now},
		{SpanID: traceTestID(907), TraceID: traceTestID(807), GroupID: "group-a", RequestID: "reused-request", Service: requesttrace.ServiceAIProxy, Stage: requesttrace.StageRequest, Status: requesttrace.StatusSuccess, StartedAt: now.Add(-15 * 24 * time.Hour), UpdatedAt: now.Add(-15 * 24 * time.Hour)},
	}
	require.NoError(t, db.Create(&fixtures).Error)

	first, err := store.FindRequests(context.Background(), TraceRequestQuery{GroupID: "group-a", RequestID: "reused-request", Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []TraceRequestCandidate{{TraceID: traceTestID(801), SpanID: traceTestID(901), StartedAt: fixtures[0].StartedAt, Status: requesttrace.StatusSuccess}}, first.Items)
	require.Equal(t, traceTestID(901), first.NextCursor)

	second, err := store.FindRequests(context.Background(), TraceRequestQuery{GroupID: "group-a", RequestID: "reused-request", AfterSpanID: first.NextCursor, Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []TraceRequestCandidate{{TraceID: traceTestID(802), SpanID: traceTestID(902), StartedAt: fixtures[1].StartedAt, Status: requesttrace.StatusError}}, second.Items)
	require.Empty(t, second.NextCursor)

	other, err := store.FindRequests(context.Background(), TraceRequestQuery{GroupID: "group-b", RequestID: "reused-request"})
	require.NoError(t, err)
	require.Len(t, other.Items, 1)
	require.Equal(t, traceTestID(803), other.Items[0].TraceID)
}

func TestTraceStoreFindRequestsRejectsUnscopedQueriesAndBoundsLimit(t *testing.T) {
	store := NewTraceStore(openTraceSQLite(t))
	require.NoError(t, store.Migrate(context.Background()))
	for _, query := range []TraceRequestQuery{{RequestID: "request"}, {GroupID: "group"}} {
		_, err := store.FindRequests(context.Background(), query)
		require.Error(t, err)
	}
	_, err := NewTraceStore(nil).FindRequests(context.Background(), TraceRequestQuery{GroupID: "group", RequestID: "request"})
	require.Error(t, err)
}

func TestTraceStoreCleanExpiredDeletesOnlyExpiredTraceRowsInBoundedBatches(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))
	require.NoError(t, db.AutoMigrate(&Log{}, &Token{}))

	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-14 * 24 * time.Hour)
	old := cutoff.Add(-time.Nanosecond)
	boundary := cutoff
	fresh := cutoff.Add(time.Nanosecond)

	oldTrace := traceTestID(800)
	require.NoError(t, db.Create(&Log{CreatedAt: old}).Error)
	require.NoError(t, db.Create(&Token{GroupID: "group-one", Name: "retained-quota", UsedAmount: 12.5, Quota: 50}).Error)
	for _, fixture := range []struct {
		sequence int
		traceID  string
		updated  time.Time
	}{
		{801, oldTrace, old},
		{802, oldTrace, old},
		{803, traceTestID(803), boundary},
		{804, traceTestID(804), fresh},
	} {
		span := traceTestSpan(fixture.sequence)
		span.TraceID = fixture.traceID
		require.NoError(t, store.Write(context.Background(), span))
		require.NoError(t, db.Model(&RequestTraceSpan{}).Where("span_id = ?", span.SpanID).UpdateColumn("updated_at", fixture.updated).Error)
		require.NoError(t, db.Model(&RequestTraceHead{}).Where("trace_id = ? AND service = ?", span.TraceID, span.Service).UpdateColumn("updated_at", fixture.updated).Error)
	}

	deleted, err := store.CleanExpired(context.Background(), now, 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)
	deleted, err = store.CleanExpired(context.Background(), now, 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)
	deleted, err = store.CleanExpired(context.Background(), now, 1)
	require.NoError(t, err)
	require.Zero(t, deleted)

	var spans []RequestTraceSpan
	require.NoError(t, db.Order("span_id").Find(&spans).Error)
	require.Equal(t, []string{traceTestID(200803), traceTestID(200804)}, traceStoredSpanIDs(spans))
	var heads []RequestTraceHead
	require.NoError(t, db.Find(&heads).Error)
	require.Len(t, heads, 2)
	var logCount, tokenCount int64
	require.NoError(t, db.Model(&Log{}).Count(&logCount).Error)
	require.NoError(t, db.Model(&Token{}).Count(&tokenCount).Error)
	require.Equal(t, int64(1), logCount)
	require.Equal(t, int64(1), tokenCount)

	for _, batchSize := range []int{0, -1, 1001} {
		_, err := store.CleanExpired(context.Background(), now, batchSize)
		require.Error(t, err)
	}

	_, err = NewTraceStore(nil).CleanExpired(context.Background(), now, 1)
	require.Error(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = store.CleanExpired(ctx, now, 1)
	require.ErrorIs(t, err, context.Canceled)
}

func TestTraceStoreCleanExpiredRemovesStaleEmptyHead(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))

	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	head := RequestTraceHead{
		TraceID: traceTestID(850), Service: requesttrace.ServiceAIProxy, GroupID: "group-one",
		UpdatedAt: now.Add(-14*24*time.Hour - time.Nanosecond),
	}
	require.NoError(t, db.Create(&head).Error)

	deleted, err := store.CleanExpired(context.Background(), now, 1)
	require.NoError(t, err)
	require.Zero(t, deleted)
	var count int64
	require.NoError(t, db.Model(&RequestTraceHead{}).Count(&count).Error)
	require.Zero(t, count)
}

func tracePageSpanIDs(page TracePage) []string {
	ids := make([]string, len(page.Items))
	for i, span := range page.Items {
		ids[i] = span.SpanID
	}
	return ids
}

func traceStoredSpanIDs(spans []RequestTraceSpan) []string {
	ids := make([]string, len(spans))
	for i, span := range spans {
		ids[i] = span.SpanID
	}
	return ids
}
