package model

import (
	"github.com/labring/aiproxy/core/common/failover"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"testing"
	"time"
)

func TestImageAttemptReservationBlocksDuplicateAndAcceptedRetry(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "attempt-cas.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&ImageTask{}, &AsyncUsageInfo{}, &Log{}))
	old := LogDB
	LogDB = db
	t.Cleanup(func() { LogDB = old })
	task, created, err := ReserveImageTask(&ImageTask{ID: "cas", Model: "m", GroupID: "g", TokenID: 1, Fingerprint: "f"}, &AsyncUsageInfo{RequestID: "cas", Price: Price{PerRequestPrice: 0.5}})
	require.NoError(t, err)
	require.True(t, created)
	task.Attempts = []ImageTaskAttempt{{ChannelID: 1, StartedAt: time.Now(), Failure: failover.Failure{Acceptance: failover.Unknown}, Decision: "in_flight", PotentialCost: "unverified"}}
	require.NoError(t, SaveImageTaskAttempt(task, 0, 1, "https://one.example"))
	require.Error(t, SaveImageTaskAttempt(task, 0, 1, "https://one.example"))
	task.Attempts[0].Failure.Acceptance = failover.Accepted
	task.Attempts[0].Decision = "accepted"
	require.NoError(t, SaveImageTaskAttempt(task, 1, 1, "https://one.example"))
	task.Attempts = append(task.Attempts, ImageTaskAttempt{ChannelID: 2})
	require.Error(t, SaveImageTaskAttempt(task, 1, 2, "https://two.example"))
	var info AsyncUsageInfo
	require.NoError(t, db.First(&info).Error)
	require.Equal(t, 1, info.ChannelID)
	require.EqualValues(t, 0.5, info.Price.PerRequestPrice)
	stored, err := GetImageTask("cas", "g", 1)
	require.NoError(t, err)
	require.Len(t, stored.Attempts, 1)
	require.Equal(t, "unverified", stored.Attempts[0].PotentialCost)
}
