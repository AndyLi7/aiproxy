package model

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestTaskTracePersistsAndResolvesFromStoredTaskIdentity(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))
	require.NoError(t, db.AutoMigrate(&AsyncUsageInfo{}))
	info := AsyncUsageInfo{GroupID: "group-a", TokenID: 7, ChannelID: 9, UpstreamID: "task-a", RequestID: "submit-a"}
	require.NoError(t, db.Create(&info).Error)
	err := store.SaveTaskTrace(context.Background(), info.ID, "group-a", "11111111111111111111111111111111", "22222222222222222222222222222222")
	require.NoError(t, err)
	resumed := NewTraceStore(db)
	link, err := resumed.ResolveTaskTrace(context.Background(), "group-a", 7, 9, "task-a", time.Now())
	require.NoError(t, err)
	require.Equal(t, info.ID, link.AsyncUsageID)
	require.Equal(t, "11111111111111111111111111111111", link.TraceID)
	require.Equal(t, "22222222222222222222222222222222", link.ParentSpanID)
}

func TestTaskTraceCleanupRemovesOnlyExpiredAssociations(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))
	now := time.Now().UTC()
	require.NoError(t, db.Create(&RequestTraceTask{AsyncUsageID: 1, ExpiresAt: now.Add(-time.Hour)}).Error)
	require.NoError(t, db.Create(&RequestTraceTask{AsyncUsageID: 2, ExpiresAt: now.Add(time.Hour)}).Error)
	removed, err := store.CleanTaskTraces(context.Background(), now, 500)
	require.NoError(t, err)
	require.EqualValues(t, 1, removed)
	var rows []RequestTraceTask
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1)
	require.Equal(t, 2, rows[0].AsyncUsageID)
}

func TestTaskTracePollSummarySurvivesDetailedSpanLimit(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	ctx := context.Background()
	require.NoError(t, store.Migrate(ctx))
	now := time.Now().UTC()
	require.NoError(t, db.Create(&RequestTraceTask{AsyncUsageID: 1, GroupID: "group-a", ExpiresAt: now.Add(time.Hour)}).Error)
	for i := 0; i < 260; i++ {
		require.NoError(t, store.RecordTaskPoll(ctx, 1, "group-a", now.Add(time.Duration(i)*time.Second)))
	}
	var link RequestTraceTask
	require.NoError(t, db.First(&link).Error)
	require.EqualValues(t, 260, link.PollCount)
	require.NotNil(t, link.FirstPollAt)
	require.NotNil(t, link.LastPollAt)
	require.True(t, link.FirstPollAt.Equal(now))
	require.True(t, link.LastPollAt.Equal(now.Add(259*time.Second)))
}
