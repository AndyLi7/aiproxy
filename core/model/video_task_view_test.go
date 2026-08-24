package model

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

func TestListGroupVideoTasksScopesOrdersAndProjectsSafeFields(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "video-tasks.db"))
	require.NoError(t, err)

	previousDB, previousLogDB := DB, LogDB
	DB, LogDB = db, db
	t.Cleanup(func() {
		DB, LogDB = previousDB, previousLogDB
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			require.NoError(t, sqlDB.Close())
		}
	})

	require.NoError(t, db.AutoMigrate(&Channel{}, &AsyncUsageInfo{}))
	require.NoError(t, db.Create(&Channel{
		ID:   7,
		Name: "private-channel-name",
		Type: ChannelTypeDoubao,
	}).Error)

	base := time.Date(2026, time.August, 24, 8, 0, 0, 0, time.UTC)
	outputAudio := true
	rows := []AsyncUsageInfo{
		{
			RequestID:       "req-completed",
			RequestAt:       base.Add(2 * time.Minute),
			Mode:            int(mode.DoubaoVideo),
			Model:           "seedance-1-5-pro",
			ChannelID:       7,
			GroupID:         "group-a",
			TokenName:       "Production key",
			UpstreamID:      "task-public-1",
			Status:          AsyncUsageStatusCompleted,
			UsageContext:    UsageContext{Resolution: "1280x720", NativeResolution: "720p", OutputAudio: &outputAudio},
			Amount:          Amount{UsedAmount: 0.42},
			PricingCurrency: "USD",
			CreatedAt:       base.Add(2 * time.Minute),
			UpdatedAt:       base.Add(3 * time.Minute),
		},
		{
			RequestID:       "req-pending",
			Mode:            int(mode.Videos),
			Model:           "veo-3",
			GroupID:         "group-a",
			Status:          AsyncUsageStatusPending,
			Amount:          Amount{UsedAmount: 99},
			PricingCurrency: "USD",
			CreatedAt:       base.Add(2 * time.Minute),
			UpdatedAt:       base.Add(4 * time.Minute),
		},
		{
			RequestID: "req-chat",
			Mode:      int(mode.ChatCompletions),
			Model:     "not-video",
			GroupID:   "group-a",
			Status:    AsyncUsageStatusCompleted,
			CreatedAt: base.Add(5 * time.Minute),
		},
		{
			RequestID: "req-other-group",
			Mode:      int(mode.VideoGenerationsJobs),
			Model:     "video-model",
			GroupID:   "group-b",
			Status:    AsyncUsageStatusFailed,
			CreatedAt: base.Add(6 * time.Minute),
		},
		{
			RequestID: "req-failed",
			Mode:      int(mode.GeminiVideo),
			Model:     "veo-failed",
			GroupID:   "group-a",
			Status:    AsyncUsageStatusFailed,
			Amount:    Amount{UsedAmount: 0.1},
			CreatedAt: base,
			UpdatedAt: base.Add(time.Minute),
		},
	}
	require.NoError(t, db.Create(&rows).Error)

	page, err := ListGroupVideoTasks("group-a", 1, 20)
	require.NoError(t, err)
	require.EqualValues(t, 3, page.Total)
	require.Len(t, page.Items, 3)

	// Equal creation timestamps are deterministically ordered by descending ID.
	require.Equal(t, "req-pending", page.Items[0].RequestID)
	require.Equal(t, "processing", page.Items[0].Status)
	require.Nil(t, page.Items[0].Amount)
	require.Empty(t, page.Items[0].Currency)
	require.Nil(t, page.Items[0].CompletedAt)

	completed := page.Items[1]
	require.Equal(t, "req-completed", completed.RequestID)
	require.Equal(t, "task-public-1", completed.PublicTaskID)
	require.Equal(t, "seedance-1-5-pro", completed.Model)
	require.Equal(t, "Production key", completed.TokenName)
	require.Equal(t, "doubao", completed.Provider)
	require.Equal(t, "1280x720", completed.Params.Size)
	require.Equal(t, "720p", completed.Params.Resolution)
	require.NotNil(t, completed.Params.GenerateAudio)
	require.True(t, *completed.Params.GenerateAudio)
	require.NotNil(t, completed.Amount)
	require.Equal(t, 0.42, *completed.Amount)
	require.Equal(t, "USD", completed.Currency)
	require.Equal(t, base.Add(2*time.Minute), completed.CreatedAt)
	require.NotNil(t, completed.CompletedAt)
	require.Equal(t, base.Add(3*time.Minute), *completed.CompletedAt)

	failed := page.Items[2]
	require.Equal(t, "failed", failed.Status)
	require.NotNil(t, failed.Amount)
	require.Equal(t, 0.1, *failed.Amount)
	require.Equal(t, "USD", failed.Currency)
	require.NotNil(t, failed.CompletedAt)
}

func TestListGroupVideoTasksRejectsUnboundedPagination(t *testing.T) {
	for _, tc := range []struct {
		name    string
		group   string
		page    int
		perPage int
	}{
		{name: "empty group", page: 1, perPage: 20},
		{name: "long group", group: string(make([]byte, 65)), page: 1, perPage: 20},
		{name: "zero page", group: "group-a", page: 0, perPage: 20},
		{name: "zero page size", group: "group-a", page: 1, perPage: 0},
		{name: "oversized page", group: "group-a", page: 10001, perPage: 20},
		{name: "oversized page size", group: "group-a", page: 1, perPage: 101},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListGroupVideoTasks(tc.group, tc.page, tc.perPage)
			require.Error(t, err)
		})
	}
}
