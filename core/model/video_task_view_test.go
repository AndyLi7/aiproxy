package model

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestListGroupVideoTasksScopesOrdersAndProjectsSafeFields(t *testing.T) {
	database := openVideoTaskViewTestDatabase(t, "video-tasks.db")
	require.NoError(t, database.AutoMigrate(&Channel{}, &AsyncUsageInfo{}))

	require.NoError(t, database.Create(&Channel{
		ID:   7,
		Name: "ark-production",
		Type: ChannelTypeDoubao,
	}).Error)

	oldest := time.Date(2026, 8, 21, 1, 0, 0, 0, time.UTC)
	newest := oldest.Add(time.Minute)
	audio := true
	rows := []AsyncUsageInfo{
		{
			RequestID:       "req-group-a-completed",
			RequestAt:       oldest,
			Mode:            int(mode.Videos),
			Model:           "seedance-1-5-pro",
			ChannelID:       7,
			GroupID:         "group-a",
			TokenID:         11,
			TokenName:       "customer-key",
			PricingCurrency: "USD",
			PricingVersion:  "release-1",
			UpstreamID:      "video-public-a",
			Status:          AsyncUsageStatusCompleted,
			UsageContext: UsageContext{
				Resolution:       "1280x720",
				NativeResolution: "720p",
				Seconds:          5,
				OutputAudio:      &audio,
			},
			Amount:    Amount{UsedAmount: 0.125},
			CreatedAt: oldest,
			UpdatedAt: oldest.Add(30 * time.Second),
		},
		{
			RequestID:  "req-group-a-processing",
			RequestAt:  newest,
			Mode:       int(mode.DoubaoVideo),
			Model:      "seedance-1-5-pro",
			ChannelID:  7,
			GroupID:    "group-a",
			TokenID:    12,
			TokenName:  "playground",
			UpstreamID: "video-public-b",
			Status:     AsyncUsageStatusPending,
			UsageContext: UsageContext{
				Resolution: "720p_16:9",
			},
			Amount:    Amount{UsedAmount: 99},
			CreatedAt: newest,
			UpdatedAt: newest,
		},
		{
			RequestID: "req-not-video",
			Mode:      int(mode.ChatCompletions),
			Model:     "text-model",
			ChannelID: 7,
			GroupID:   "group-a",
			Status:    AsyncUsageStatusCompleted,
			CreatedAt: newest.Add(time.Minute),
			UpdatedAt: newest.Add(time.Minute),
		},
		{
			RequestID: "req-other-tenant",
			Mode:      int(mode.Videos),
			Model:     "seedance-1-5-pro",
			ChannelID: 7,
			GroupID:   "group-b",
			Status:    AsyncUsageStatusCompleted,
			CreatedAt: newest.Add(2 * time.Minute),
			UpdatedAt: newest.Add(2 * time.Minute),
		},
	}
	require.NoError(t, database.Create(&rows).Error)

	page, err := ListGroupVideoTasks("group-a", 1, 20)
	require.NoError(t, err)
	require.EqualValues(t, 2, page.Total)
	require.Len(t, page.Items, 2)

	processing := page.Items[0]
	require.Equal(t, "req-group-a-processing", processing.RequestID)
	require.Equal(t, "video-public-b", processing.PublicTaskID)
	require.Equal(t, "processing", processing.Status)
	require.Nil(t, processing.Amount)
	require.Nil(t, processing.CompletedAt)

	completed := page.Items[1]
	require.NotEmpty(t, completed.ID)
	require.Equal(t, "req-group-a-completed", completed.RequestID)
	require.Equal(t, "seedance-1-5-pro", completed.Model)
	require.Equal(t, "customer-key", completed.TokenName)
	require.Equal(t, "doubao", completed.Provider)
	require.Equal(t, "completed", completed.Status)
	require.NotNil(t, completed.Amount)
	require.InDelta(t, 0.125, *completed.Amount, 0.000001)
	require.Equal(t, "USD", completed.Currency)
	require.Equal(t, "1280x720", completed.Params.Size)
	require.Equal(t, "720p", completed.Params.Resolution)
	require.Equal(t, 5, completed.Params.Seconds)
	require.Equal(t, &audio, completed.Params.GenerateAudio)
	require.NotNil(t, completed.CompletedAt)
	require.Equal(t, oldest.Add(30*time.Second), *completed.CompletedAt)
}

func TestListGroupVideoTasksRejectsUnboundedPagination(t *testing.T) {
	tests := []struct {
		name    string
		group   string
		page    int
		perPage int
		message string
	}{
		{name: "empty group", group: "", page: 1, perPage: 20, message: "invalid group"},
		{name: "oversized group", group: strings.Repeat("g", 65), page: 1, perPage: 20, message: "invalid group"},
		{name: "zero page", group: "group-a", page: 0, perPage: 20, message: "invalid pagination"},
		{name: "oversized page", group: "group-a", page: 10_001, perPage: 20, message: "invalid pagination"},
		{name: "zero page size", group: "group-a", page: 1, perPage: 0, message: "invalid pagination"},
		{name: "oversized page size", group: "group-a", page: 1, perPage: 101, message: "invalid pagination"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ListGroupVideoTasks(tt.group, tt.page, tt.perPage)
			require.EqualError(t, err, tt.message)
		})
	}
}

func TestFindGroupVideoTaskByRequestIDScopesAndProjectsSafeFields(t *testing.T) {
	database := openVideoTaskViewTestDatabase(t, "video-task-by-request.db")
	require.NoError(t, database.AutoMigrate(&Channel{}, &AsyncUsageInfo{}))
	require.NoError(t, database.Create(&Channel{
		ID:   8,
		Name: "ark-production",
		Type: ChannelTypeDoubao,
	}).Error)
	require.NoError(t, database.Create(&[]AsyncUsageInfo{
		{
			RequestID:       "req-video-1",
			Mode:            int(mode.Videos),
			ChannelID:       8,
			GroupID:         "group-a",
			Status:          AsyncUsageStatusPending,
			BaseURL:         "https://upstream-secret.invalid",
			ProcessingToken: "processing-secret",
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
		},
		{
			RequestID: "req-video-1",
			Mode:      int(mode.Videos),
			ChannelID: 8,
			GroupID:   "group-b",
			Status:    AsyncUsageStatusCompleted,
			CreatedAt: time.Now().Add(time.Second),
			UpdatedAt: time.Now().Add(time.Second),
		},
	}).Error)

	got, err := FindGroupVideoTaskByRequestID("group-a", "req-video-1")
	require.NoError(t, err)
	require.Equal(t, "req-video-1", got.RequestID)
	require.Equal(t, "processing", got.Status)
	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "base_url")
	require.NotContains(t, string(encoded), "processing_token")
	require.NotContains(t, string(encoded), "prompt")
}

func TestFindGroupVideoTaskByRequestIDRejectsWrongGroup(t *testing.T) {
	database := openVideoTaskViewTestDatabase(t, "video-task-by-request-wrong-group.db")
	require.NoError(t, database.AutoMigrate(&Channel{}, &AsyncUsageInfo{}))
	require.NoError(t, database.Create(&Channel{ID: 8, Type: ChannelTypeDoubao}).Error)
	require.NoError(t, database.Create(&AsyncUsageInfo{
		RequestID: "req-only-group-a",
		Mode:      int(mode.Videos),
		ChannelID: 8,
		GroupID:   "group-a",
		Status:    AsyncUsageStatusPending,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}).Error)

	got, err := FindGroupVideoTaskByRequestID("group-b", "req-only-group-a")
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	require.Nil(t, got)
}

func openVideoTaskViewTestDatabase(t *testing.T, name string) *gorm.DB {
	t.Helper()
	previousDB := DB
	previousLogDB := LogDB
	database, err := OpenSQLite(filepath.Join(t.TempDir(), name))
	require.NoError(t, err)
	underlying, err := database.DB()
	require.NoError(t, err)
	DB = database
	LogDB = database
	t.Cleanup(func() {
		DB = previousDB
		LogDB = previousLogDB
		require.NoError(t, underlying.Close())
	})
	return database
}
