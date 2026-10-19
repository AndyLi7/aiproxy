//nolint:testpackage // White-box regression exercises private model or runtime invariants.
package model

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAdmissionSQLiteClockPreservesEveryMillisecond(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "clock.db"))
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })

	// 999 used to become 998 via Julian-day floating-point truncation.
	// Exercise all fractional values and both sides of a second boundary.
	base := time.Date(2026, time.October, 19, 0, 0, 0, 0, time.UTC)

	query := "SELECT " + strings.ReplaceAll(admissionClockSQL(db), "'now'", "?")
	for i := -1; i <= 1000; i++ {
		stamp := base.Add(time.Duration(i) * time.Millisecond)
		input := stamp.Format("2006-01-02 15:04:05.000")

		var got int64
		require.NoError(t, db.Raw(query, input, input).Scan(&got).Error)
		require.Equal(t, stamp.UnixMilli(), got, "timestamp %s", input)
	}
	// Both reads and final SQL guards must share the same clock expression.
	require.Equal(t, sqliteAdmissionClockSQL, admissionClockSQL(db))

	before := time.Now().UnixMilli()
	now, err := channelAdmissionTime(db)
	require.NoError(t, err)
	require.GreaterOrEqual(t, now.UnixMilli(), before)
	require.LessOrEqual(t, now.UnixMilli(), time.Now().UnixMilli())
}
