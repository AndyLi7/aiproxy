//nolint:testpackage // These fixtures verify internal metering and persistence boundaries.
package model

import (
	"path/filepath"
	"testing"
	"time"

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

		var row Log
		require.NoError(t, db.First(&row, info.LogID).Error)
		require.Equal(t, got.Status, row.AsyncUsageStatus)

		switch state {
		case "pending":
			require.Equal(t, AsyncUsageStatusMeasurementPending, got.Status)
		case "failed":
			require.Equal(t, AsyncUsageStatusFailed, got.Status)
		default:
			// Key usage P2 (plan C3): a complete zero bill is Pending so the
			// outbox reports its measured-zero receipt. Old: Completed, never
			// claimed, and the receipt waited for a customer read.
			require.Equal(t, AsyncUsageStatusPending, got.Status)
			require.Equal(t, AsyncUsageStatusPending, row.AsyncUsageStatus)
		}
	}

	pending, err := GetPendingAsyncUsages(10)
	require.NoError(t, err)
	require.Empty(t, pending, "the outbox claims nothing before its first poll delay")

	due, err := GetPendingAsyncUsagesDue(10, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, due, 1, "only the complete zero bill is claimable")
	require.Equal(t, "complete", due[0].RequestID)

	// Non-final attempt evidence keeps the old switch: never claimable.
	for _, tc := range []struct {
		name   string
		micros int64
		used   float64
		want   AsyncUsageStatus
	}{
		{"evidence zero", 0, 0, AsyncUsageStatusCompleted},
		{"evidence positive", 5, 0.000005, AsyncUsageStatusFailed},
	} {
		micros := tc.micros
		evidence := &AsyncUsageInfo{
			RequestID: tc.name,
			Amount: Amount{UsedAmount: tc.used, ImageBillingResult: &ImageBillingResult{
				State: "complete", AmountMicros: &micros, Lines: []ImageBillingLine{},
			}},
		}
		require.NoError(t, CreateMeasuredImageAttemptEvidence(evidence), tc.name)

		var got AsyncUsageInfo
		require.NoError(t, db.First(&got, evidence.ID).Error)
		require.Equal(t, tc.want, got.Status, tc.name)
		require.Zero(t, got.LogID, tc.name)
	}
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
