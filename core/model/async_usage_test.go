package model

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAsyncUsageCapabilitySurvivesRetryAndCompletion(t *testing.T) {
	database, err := OpenSQLite(filepath.Join(t.TempDir(), "async-capability.db"))
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&AsyncUsageInfo{}))

	underlying, err := database.DB()
	require.NoError(t, err)
	previousLogDB := LogDB
	LogDB = database
	t.Cleanup(func() {
		LogDB = previousLogDB
		require.NoError(t, underlying.Close())
	})

	now := time.Now()
	info := &AsyncUsageInfo{
		RequestID:  "req-capability",
		Model:      "bytedance/seedance-2.0",
		Capability: string(ModelCapabilityImageToVideo),
		NextPollAt: now.Add(-time.Second),
	}
	require.NoError(t, CreateAsyncUsageInfo(info))

	claimed, err := TryClaimAsyncUsageInfo(info, "claim-retry", now.Add(time.Minute), now)
	require.NoError(t, err)
	require.True(t, claimed)
	info.RetryCount = 1
	info.NextPollAt = now.Add(2 * time.Minute)
	require.NoError(t, RetryClaimedAsyncUsageInfo(info))

	var retried AsyncUsageInfo
	require.NoError(t, database.First(&retried, info.ID).Error)
	require.Equal(t, "bytedance/seedance-2.0", retried.Model)
	require.Equal(t, string(ModelCapabilityImageToVideo), retried.Capability)

	claimed, err = TryClaimAsyncUsageInfo(
		&retried,
		"claim-complete",
		now.Add(4*time.Minute),
		now.Add(3*time.Minute),
	)
	require.NoError(t, err)
	require.True(t, claimed)
	completed, err := CompleteClaimedAsyncUsageInfo(
		&retried,
		Usage{OutputTokens: 10, TotalTokens: 10},
		UsageContext{Seconds: 5},
		Amount{UsedAmount: 0.01},
	)
	require.NoError(t, err)
	require.True(t, completed)

	var got AsyncUsageInfo
	require.NoError(t, database.First(&got, info.ID).Error)
	require.Equal(t, AsyncUsageStatusCompleted, got.Status)
	require.Equal(t, "bytedance/seedance-2.0", got.Model)
	require.Equal(t, string(ModelCapabilityImageToVideo), got.Capability)
}
