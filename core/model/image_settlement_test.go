//nolint:testpackage // These fixtures verify internal metering and persistence boundaries.
package model

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMeasuredSettlementAtomicAndTerminalStates(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "measured.db"))
	require.NoError(t, err)

	old := LogDB
	LogDB = db
	t.Cleanup(func() { LogDB = old; sql, _ := db.DB(); _ = sql.Close() })
	require.NoError(t, db.AutoMigrate(&Log{}, &AsyncUsageInfo{}))

	for _, state := range []string{"complete", "pending", "failed"} {
		zero := int64(0)

		r := &ImageBillingResult{State: state, AmountMicros: &zero, Lines: []ImageBillingLine{}}
		if state == "pending" {
			r.AmountMicros = nil
		}

		info := &AsyncUsageInfo{
			RequestID: state,
			Price:     Price{ImageBilling: &ImageBillingPolicy{Version: 1, Scenario: "generation"}},
			Amount:    Amount{ImageBillingResult: r},
		}
		entry := &Log{RequestID: EmptyNullString(state)}
		require.NoError(t, CreateMeasuredImageSettlement(entry, info))
		require.Positive(t, info.LogID)

		var got AsyncUsageInfo
		require.NoError(t, db.First(&got, info.ID).Error)
		require.True(t, got.MeasuredImage)

		switch state {
		case "pending":
			require.Equal(t, AsyncUsageStatusMeasurementPending, got.Status)
		case "failed":
			require.Equal(t, AsyncUsageStatusFailed, got.Status)
		default:
			require.Equal(t, AsyncUsageStatusCompleted, got.Status)
		}
	}

	pending, err := GetPendingAsyncUsages(10)
	require.NoError(t, err)
	require.Empty(t, pending)
	// A failed outbox insertion must roll back the paired log.
	require.NoError(t, db.Migrator().DropTable(&AsyncUsageInfo{}))
	require.Error(
		t,
		CreateMeasuredImageSettlement(
			&Log{RequestID: "rollback"},
			&AsyncUsageInfo{
				Amount: Amount{ImageBillingResult: &ImageBillingResult{State: "pending"}},
			},
		),
	)

	var count int64
	require.NoError(t, db.Model(&Log{}).Where("request_id = ?", "rollback").Count(&count).Error)
	require.Zero(t, count)
}
