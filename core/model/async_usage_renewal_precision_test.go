//nolint:testpackage // White-box regression exercises private model or runtime invariants.
package model

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// Regression reproduced from ASYNC-RENEW-R1: actual GORM MySQL model/schema/SQL with a recording
// driver that applies DATETIME(3) storage precision. This is NOT a MySQL instance
// test and does not establish MySQL engine locking or runtime comparison behavior.
type renewalPrecisionRecord struct {
	renewalRecord
	truncate    bool
	stored      time.Time
	proposed    time.Time
	checked     time.Time
	finalExpiry bool
	wrote       bool
}
type renewalPrecisionConnector struct{ r *renewalPrecisionRecord }

func (c renewalPrecisionConnector) Driver() driver.Driver { return queueSQLDriver{} }
func (c renewalPrecisionConnector) Connect(context.Context) (driver.Conn, error) {
	return &renewalPrecisionConn{
		renewalConn: renewalConn{
			queueSQLConn: queueSQLConn{&c.r.queueSQLRecorder},
			r:            &c.r.renewalRecord,
		},
		p: c.r,
	}, nil
}

type renewalPrecisionConn struct {
	renewalConn
	p *renewalPrecisionRecord
}

// A successful transaction advances the persisted deadline used by the next
// renewal; rollback preserves the original one.
func (c *renewalPrecisionConn) Begin() (driver.Tx, error) {
	c.r.events = append(c.r.events, "begin")
	c.p.wrote = false
	return &renewalPrecisionTx{queueSQLTx: queueSQLTx{&c.r.queueSQLRecorder}, p: c.p}, nil
}

type renewalPrecisionTx struct {
	queueSQLTx
	p *renewalPrecisionRecord
}

func (tx *renewalPrecisionTx) Commit() error {
	if tx.p.wrote {
		tx.p.old = tx.p.stored
	}
	return tx.queueSQLTx.Commit()
}

func (tx *renewalPrecisionTx) Rollback() error {
	tx.p.stored = tx.p.old
	return tx.queueSQLTx.Rollback()
}

func (c *renewalPrecisionConn) ExecContext(
	ctx context.Context,
	q string,
	args []driver.NamedValue,
) (driver.Result, error) {
	if strings.HasPrefix(q, "UPDATE") {
		c.p.wrote = true

		proposed, ok := args[0].Value.(time.Time)
		if !ok {
			return nil, errors.New("expected deadline time")
		}

		c.p.proposed = proposed

		c.p.stored = c.p.proposed.Round(time.Millisecond)
		if c.p.truncate {
			c.p.stored = c.p.proposed.Truncate(time.Millisecond)
		}
	}

	return c.queueSQLConn.ExecContext(ctx, q, args)
}

func (c *renewalPrecisionConn) QueryContext(
	ctx context.Context,
	q string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	if strings.Contains(q, "count(*)") {
		for _, a := range args {
			if v, ok := a.Value.(time.Time); ok {
				c.p.checked = v
			}
		}

		clock := time.Now().UnixMilli()
		if c.p.finalExpiry {
			clock = c.r.old.UnixMilli() + 1
		}

		priorDeadline, ok := args[len(args)-1].Value.(int64)
		if !ok {
			return nil, errors.New("expected prior deadline integer")
		}

		c.r.expired = !c.p.stored.Equal(c.p.checked) || priorDeadline <= clock
	}

	return c.renewalConn.QueryContext(ctx, q, args)
}

func TestAsyncUsageRenewalMySQLDatetime3KeepsValidLease(t *testing.T) {
	for _, tc := range []struct {
		name     string
		micros   int
		truncate bool
	}{
		{"aligned-control", 123000, false},
		{"round-down", 123456, false},
		{"round-up", 123789, false},
		{"truncate-mode", 123456, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &renewalPrecisionRecord{
				renewalRecord: renewalRecord{
					old: time.Now().UTC().Truncate(time.Second).Add(time.Minute),
				},
				truncate: tc.truncate,
			}

			pool := sql.OpenDB(renewalPrecisionConnector{r})
			defer pool.Close()

			db, err := gorm.Open(
				mysql.New(mysql.Config{Conn: pool, SkipInitializeWithVersion: true}),
				&gorm.Config{DisableAutomaticPing: true},
			)
			require.NoError(t, err)

			stmt := &gorm.Statement{DB: db}
			require.NoError(t, stmt.Parse(&AsyncUsageInfo{}))
			columnType := db.DataTypeOf(stmt.Schema.LookUpField("NextPollAt"))
			require.Equal(t, "datetime(3) NULL", columnType)

			oldDB := LogDB

			LogDB = db
			defer func() { LogDB = oldDB }()

			proposal := r.old.Add(time.Minute + time.Duration(tc.micros)*time.Microsecond)
			ok, err := RenewAsyncUsageClaimContext(t.Context(), 1, "token", proposal)
			require.NoError(t, err)
			t.Logf(
				"GORM column=%s proposal=%s stored=%s final-bound=%s events=%v",
				columnType,
				r.proposed.Format(time.RFC3339Nano),
				r.stored.Format(time.RFC3339Nano),
				r.checked.Format(time.RFC3339Nano),
				r.events,
			)
			require.True(
				t,
				ok,
				"live original owner must renew across storage precision without rolling back",
			)
			require.Equal(t, "commit", r.events[len(r.events)-1])
		})
	}
}

func renewalPrecisionDB(t *testing.T, r *renewalPrecisionRecord) {
	t.Helper()

	pool := sql.OpenDB(renewalPrecisionConnector{r})
	t.Cleanup(func() { require.NoError(t, pool.Close()) })

	db, err := gorm.Open(
		mysql.New(mysql.Config{Conn: pool, SkipInitializeWithVersion: true}),
		&gorm.Config{DisableAutomaticPing: true},
	)
	require.NoError(t, err)

	statement := &gorm.Statement{DB: db}
	require.NoError(t, statement.Parse(&AsyncUsageInfo{}))
	require.Equal(t, "datetime(3) NULL", db.DataTypeOf(statement.Schema.LookUpField("NextPollAt")))

	previous := LogDB
	LogDB = db
	t.Cleanup(func() { LogDB = previous })
}

func TestAsyncUsageRenewalMySQLPrecisionContinuousAndBoundaries(t *testing.T) {
	for _, truncate := range []bool{false, true} {
		mode := "round"
		if truncate {
			mode = "TIME_TRUNCATE_FRACTIONAL"
		}

		for _, scenario := range []string{"continuous", "equal-after-normalization", "shorter", "old-deadline-after-write", "cancelled"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				old := time.Now().
					UTC().
					Truncate(time.Second).
					Add(time.Minute + 123*time.Millisecond)
				r := &renewalPrecisionRecord{
					renewalRecord: renewalRecord{old: old},
					truncate:      truncate,
				}
				renewalPrecisionDB(t, r)

				proposal := old.Add(time.Minute + 456*time.Microsecond)

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				switch scenario {
				case "equal-after-normalization":
					proposal = old.Add(999 * time.Microsecond)
				case "shorter":
					proposal = old.Add(-time.Microsecond)
				case "old-deadline-after-write":
					r.finalExpiry = true
				case "cancelled":
					cancel()
				}

				ok, err := RenewAsyncUsageClaimContext(ctx, 1, "token", proposal)
				if scenario == "cancelled" {
					require.ErrorIs(t, err, context.Canceled)
					require.False(t, ok)
					require.Equal(t, old, r.old)
					return
				}

				require.NoError(t, err)

				if scenario == "continuous" {
					require.True(t, ok)
					require.True(t, r.old.Equal(proposal.Truncate(time.Millisecond)))
					require.True(t, r.proposed.Equal(r.checked))
					require.True(t, r.stored.Equal(r.checked))
					// The second call must see the first committed storage value, not a fake
					// fixed count or the initial old deadline from the fixture.
					previous := r.old
					proposal = r.old.Add(time.Minute + 789*time.Microsecond)
					ok, err = RenewAsyncUsageClaimContext(ctx, 1, "token", proposal)
					require.NoError(t, err)
					require.True(t, ok)
					require.True(t, r.old.Equal(proposal.Truncate(time.Millisecond)))
					require.True(t, r.old.After(previous))
					require.True(t, r.proposed.Equal(r.checked))
					require.True(t, r.stored.Equal(r.checked))
					require.Zero(t, r.checked.Nanosecond()%int(time.Millisecond))

					return
				}

				require.False(t, ok)
				require.True(t, r.old.Equal(old))

				if scenario == "old-deadline-after-write" {
					require.Equal(t, "rollback", r.events[len(r.events)-1])
					args := r.checks[len(r.checks)-1]
					require.Equal(t, old.UnixMilli(), args[len(args)-1].Value)
				} else {
					require.NotContains(
						t,
						r.events,
						"update",
						"normalization must precede strict extension check",
					)
				}
			})
		}
	}
}
