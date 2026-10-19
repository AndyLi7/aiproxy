package model

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// Recording driver only: verifies MySQL SQL/bound deadlines/rollback flow, not
// MySQL engine locking or time behavior. No MySQL instance is contacted.
type renewalRecord struct {
	queueSQLRecorder
	old     time.Time
	expired bool
	checks  [][]driver.NamedValue
}
type renewalConnector struct{ r *renewalRecord }

func (c renewalConnector) Connect(context.Context) (driver.Conn, error) {
	return &renewalConn{queueSQLConn: queueSQLConn{&c.r.queueSQLRecorder}, r: c.r}, nil
}
func (c renewalConnector) Driver() driver.Driver { return queueSQLDriver{} }

type renewalConn struct {
	queueSQLConn
	r *renewalRecord
}
type renewalRows struct {
	names  []string
	values []driver.Value
	read   bool
}

func (r *renewalRows) Columns() []string { return r.names }
func (r *renewalRows) Close() error      { return nil }
func (r *renewalRows) Next(v []driver.Value) error {
	if r.read {
		return io.EOF
	}

	r.read = true
	copy(v, r.values)

	return nil
}

func (c *renewalConn) QueryContext(
	_ context.Context,
	q string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	c.r.events = append(c.r.events, "query")
	c.r.queries = append(c.r.queries, q)

	c.r.checks = append(c.r.checks, args)
	if strings.Contains(q, "FOR UPDATE") {
		return &renewalRows{
			names:  []string{"id", "status", "processing_token", "next_poll_at"},
			values: []driver.Value{int64(1), int64(AsyncUsageStatusPending), "token", c.r.old},
		}, nil
	}

	if strings.Contains(q, "count(*)") {
		count := int64(1)
		if c.r.expired {
			count = 0
		}

		return &renewalRows{names: []string{"count"}, values: []driver.Value{count}}, nil
	}

	return &renewalRows{
		names:  []string{"clock"},
		values: []driver.Value{time.Now().UnixMilli()},
	}, nil
}

func TestAsyncUsageRenewalMySQLSQLAndRollback(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "final-expiry"}[expired], func(t *testing.T) {
			record := &renewalRecord{
				old:     time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond),
				expired: expired,
			}

			pool := sql.OpenDB(renewalConnector{record})
			defer pool.Close()

			db, err := gorm.Open(
				mysql.New(mysql.Config{Conn: pool, SkipInitializeWithVersion: true}),
				&gorm.Config{DisableAutomaticPing: true},
			)
			require.NoError(t, err)

			old := LogDB

			LogDB = db
			defer func() { LogDB = old }()

			ok, err := RenewAsyncUsageClaimContext(
				t.Context(),
				1,
				"token",
				record.old.Add(time.Minute),
			)
			require.NoError(t, err)
			require.Equal(t, !expired, ok)

			final := "commit"
			if expired {
				final = "rollback"
			}

			require.Equal(
				t,
				[]string{"begin", "query", "query", "update", "query", final},
				record.events,
			)
			require.Contains(t, record.queries[0], "FOR UPDATE")
			require.Contains(t, record.queries[1], "UNIX_TIMESTAMP(CURRENT_TIMESTAMP(6))")
			require.Contains(t, record.queries[3], "UNIX_TIMESTAMP(CURRENT_TIMESTAMP(6))")
			require.Contains(t, record.queries[3], "processing_token")
			require.Contains(t, record.queries[3], "next_poll_at")
			checks := record.checks[len(record.checks)-1]
			require.Equal(
				t,
				record.old.UnixMilli(),
				checks[len(checks)-1].Value,
				"write check must bind old deadline, never proposal",
			)
		})
	}
}
