//nolint:testpackage // Reuses the recording SQL driver; no MySQL server is contacted.
package model

import (
	"context"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"testing"
)

func TestMySQLNativeRecoveryFenceUsesDatabaseClock(t *testing.T) {
	db := queueMySQLRecordingDB(t, &queueSQLRecorder{})
	db = db.WithContext(WithNativeRecoveryOwner(context.Background(), "claim-token"))
	statement := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return nativeRecoveryFence(tx.Model(&NativeTask{})).Where("id = ?", "task").UpdateColumn("status", "completed")
	})
	require.Contains(t, statement, "recovery_owner = 'claim-token'")
	require.Contains(t, statement, "recovery_until > UNIX_TIMESTAMP()")
	require.NotContains(t, statement, "julianday")
	require.NotContains(t, statement, "clock_timestamp")
}
