//nolint:testpackage
package model

import (
	"context"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

// The aggregate statements run on Postgres in production, where request_at is
// timestamptz and SUM returns bigint.
func TestUsageStatsPostgres(t *testing.T) {
	withTestPostgresStoreDB(t, func() {
		require.NoError(t, LogDB.AutoMigrate(&Log{}))

		now := time.Now()
		last := now.Add(-time.Hour)

		for _, entry := range []Log{
			{GroupID: "g", TokenID: 3, TokenName: "prod", Code: 200, RequestSource: "", RequestAt: last},
			{GroupID: "g", TokenID: 3, TokenName: "prod", Code: 502, FailureStage: FailureStageUpstream, RequestAt: now.Add(-2 * time.Hour)},
			{GroupID: "g", TokenID: 4, Code: 202, AsyncUsageStatus: AsyncUsageStatusPending, Mode: int(mode.NativeTasks), Endpoint: "POST /v1/model-tasks", RequestSource: RequestSourcePlayground, RequestAt: now.Add(-30 * time.Hour)},
			{GroupID: "g", TokenID: 3, Code: 404, Endpoint: "GET /v1/model-tasks/x", RequestAt: now.Add(-time.Minute)},
			{GroupID: "g", TokenID: 5, TokenName: "idle", Code: 200, RequestAt: now.Add(-60 * 24 * time.Hour)},
		} {
			if entry.Mode == 0 && entry.Endpoint == "" {
				entry.Mode = int(mode.ChatCompletions)
			}

			entry.CreatedAt = entry.RequestAt.Add(time.Second)
			require.NoError(t, LogDB.Create(&entry).Error)
		}

		start := now.Add(-30 * 24 * time.Hour)

		totals, err := GetGroupCallTotals(
			context.Background(),
			"g",
			start,
			now,
			now,
			CallUnitRequests,
		)
		require.NoError(t, err)

		lastMS := last.UnixMilli()
		require.Equal(t, CallCounts{
			Requests: 3, Succeeded: 1, Failed: 1, Unknown: 1, LastRequestAt: &lastMS,
		}, totals.Totals)
		require.Len(t, totals.BySource, 2)
		require.Equal(t, RequestSourceAPI, totals.BySource[0].Source)
		require.Equal(t, RequestSourcePlayground, totals.BySource[1].Source)

		keys, err := GetGroupKeyCallStats(
			context.Background(),
			"g",
			[]int{3, 5},
			start,
			now,
			now,
			CallUnitRequests,
		)
		require.NoError(t, err)
		require.Len(t, keys, 2)
		require.Equal(
			t,
			CallCounts{Requests: 2, Succeeded: 1, Failed: 1, LastRequestAt: &lastMS},
			keys[0].CallCounts,
		)
		require.Equal(t, "prod", keys[0].LastTokenName)
		require.Zero(t, keys[1].Requests)
		require.Equal(t, "idle", keys[1].LastTokenName)
		require.Equal(t, now.Add(-60*24*time.Hour).UnixMilli(), *keys[1].LastRequestAt)
	})
}
