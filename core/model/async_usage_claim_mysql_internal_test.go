package model

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// SQL recording only: no MySQL instance or engine lock assertion. A persisted
// DATETIME(3) lease authorizes a non-aligned next schedule without comparing
// the caller's new schedule to the old lease on final verification.
func TestAsyncUsageClaimReceiptMySQLClockAndPrecision(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "live", true: "expired-after-write"}[expired],
			func(t *testing.T) {
				r := &renewalRecord{
					old:     time.Now().UTC().Add(time.Minute).Truncate(time.Millisecond),
					expired: expired,
				}

				pool := sql.OpenDB(renewalConnector{r})
				defer pool.Close()

				db, err := gorm.Open(
					mysql.New(mysql.Config{Conn: pool, SkipInitializeWithVersion: true}),
					&gorm.Config{DisableAutomaticPing: true},
				)
				require.NoError(t, err)

				old := LogDB

				LogDB = db
				defer func() { LogDB = old }()

				err = TouchClaimedAsyncUsageInfo(
					&AsyncUsageInfo{
						ID:              1,
						ProcessingToken: "token",
						NextPollAt:      r.old.Add(time.Minute + 456*time.Microsecond),
					},
				)
				if expired {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}

				final := "commit"
				if expired {
					final = "rollback"
				}

				require.Equal(
					t,
					[]string{"begin", "query", "query", "update", "query", final},
					r.events,
				)
				require.Contains(t, r.queries[0], "FOR UPDATE")
				require.Contains(t, r.queries[2], "next_poll_at=?")
				require.Contains(t, r.queries[2], "UNIX_TIMESTAMP(CURRENT_TIMESTAMP(6))")
				require.Contains(t, r.queries[3], "UNIX_TIMESTAMP(CURRENT_TIMESTAMP(6))")
				require.NotContains(t, r.queries[3], "next_poll_at")
				checks := r.checks[len(r.checks)-1]
				require.Equal(t, r.old.UnixMilli(), checks[len(checks)-1].Value)
			},
		)
	}
}
