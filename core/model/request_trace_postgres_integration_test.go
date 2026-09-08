package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestTraceStorePostgresMatchesLifecycleAfterTimestampPrecisionRoundTrip(t *testing.T) {
	db := openTracePostgres(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))

	started := traceTestSpan(500)
	started.StartedAt = time.Date(2026, 9, 8, 1, 2, 3, 123456789, time.UTC)
	require.NoError(t, store.Write(context.Background(), started))

	completed := started
	completed.Status = requesttrace.StatusSuccess
	completed.Revision = 2
	endedAt := started.StartedAt.Add(987654321 * time.Nanosecond)
	duration := 987.654321
	completed.EndedAt = &endedAt
	completed.DurationMS = &duration
	require.NoError(t, store.Write(context.Background(), completed))

	var persisted RequestTraceSpan
	require.NoError(t, db.First(&persisted, "span_id = ?", started.SpanID).Error)
	require.Equal(t, requesttrace.StatusSuccess, persisted.Status)
	require.Equal(t, 2, persisted.Revision)
	require.InDelta(t, duration, *persisted.DurationMS, 0.000001)
}

func TestTraceStorePostgresSerializesConcurrent256thAnd257thSpan(t *testing.T) {
	db := openTracePostgres(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))

	for i := 1; i <= 255; i++ {
		span := traceTestSpan(1000 + i)
		span.TraceID = traceTestID(999)
		require.NoError(t, store.Write(context.Background(), span))
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(sequence int) {
			defer wg.Done()
			span := traceTestSpan(sequence)
			span.TraceID = traceTestID(999)
			<-start
			errs <- store.Write(context.Background(), span)
		}(2000 + i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	var head RequestTraceHead
	require.NoError(t, db.First(&head, "trace_id = ? AND service = ?", traceTestID(999), "aiproxy").Error)
	require.Equal(t, 256, head.SpanCount)
	require.True(t, head.Truncated)

	var spanCount int64
	require.NoError(t, db.Model(&RequestTraceSpan{}).
		Where("trace_id = ? AND service = ?", traceTestID(999), "aiproxy").Count(&spanCount).Error)
	require.Equal(t, int64(256), spanCount)
}

func openTracePostgres(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TRACE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TRACE_TEST_POSTGRES_DSN is not set; skipping real PostgreSQL trace-store race test")
	}

	admin, err := OpenPostgreSQL(dsn)
	require.NoError(t, err)
	adminSQL, err := admin.DB()
	require.NoError(t, err)

	random := make([]byte, 8)
	_, err = rand.Read(random)
	require.NoError(t, err)
	schema := "request_trace_test_" + hex.EncodeToString(random)
	require.NoError(t, admin.Exec(fmt.Sprintf("CREATE SCHEMA %s", schema)).Error)

	db, err := OpenPostgreSQL(dsn + " search_path=" + schema)
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, sqlDB.Close())
		require.NoError(t, admin.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE", schema)).Error)
		require.NoError(t, adminSQL.Close())
	})

	return db
}
