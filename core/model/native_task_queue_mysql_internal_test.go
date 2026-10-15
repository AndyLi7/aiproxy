//nolint:testpackage // Verifies the dialect predicate and real submission transaction.
package model

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// A recording SQL driver verifies generated MySQL SQL and transaction order.
// It does not execute MySQL or claim to prove MySQL's runtime locking behavior.
type queueSQLRecorder struct {
	events  []string
	queries []string
	lockErr error
}
type queueSQLConnector struct{ record *queueSQLRecorder }
type queueSQLDriver struct{}
type queueSQLConn struct{ record *queueSQLRecorder }
type queueSQLTx struct{ record *queueSQLRecorder }
type queueSQLRows struct{ read bool }

func (c queueSQLConnector) Connect(context.Context) (driver.Conn, error) {
	return &queueSQLConn{c.record}, nil
}
func (c queueSQLConnector) Driver() driver.Driver       { return queueSQLDriver{} }
func (queueSQLDriver) Open(string) (driver.Conn, error) { return nil, errors.New("connector required") }
func (c *queueSQLConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c *queueSQLConn) Close() error { return nil }
func (c *queueSQLConn) Begin() (driver.Tx, error) {
	c.record.events = append(c.record.events, "begin")
	return &queueSQLTx{c.record}, nil
}
func (c *queueSQLConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	c.record.events = append(c.record.events, "query")
	c.record.queries = append(c.record.queries, q)
	if c.record.lockErr != nil {
		return nil, c.record.lockErr
	}
	return &queueSQLRows{}, nil
}
func (c *queueSQLConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.record.events = append(c.record.events, "update")
	c.record.queries = append(c.record.queries, q)
	return driver.RowsAffected(1), nil
}
func (t *queueSQLTx) Commit() error { t.record.events = append(t.record.events, "commit"); return nil }
func (t *queueSQLTx) Rollback() error {
	t.record.events = append(t.record.events, "rollback")
	return nil
}
func (*queueSQLRows) Columns() []string { return []string{"id"} }
func (*queueSQLRows) Close() error      { return nil }
func (r *queueSQLRows) Next(values []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	values[0] = "task"
	return nil
}

func queueMySQLRecordingDB(t *testing.T, record *queueSQLRecorder) *gorm.DB {
	pool := sql.OpenDB(queueSQLConnector{record})
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: pool, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	require.NoError(t, err)
	return db
}

func TestMySQLNativeDispatchUsesLockThenFreshPredicate(t *testing.T) {
	record := &queueSQLRecorder{}
	db := queueMySQLRecordingDB(t, record)
	require.NoError(t, StartNativeDispatchSubmission(db, "task", "group", 1, "owner", time.Now()))
	require.Equal(t, []string{"begin", "query", "update", "commit"}, record.events)
	require.Contains(t, record.queries[0], "FOR UPDATE")
	require.NotContains(t, record.queries[0], "UTC_TIMESTAMP")
	require.Contains(t, record.queries[1], "dispatch_until > UTC_TIMESTAMP(6)")
	require.Contains(t, record.queries[1], "dispatch_owner = ?")
	require.Contains(t, record.queries[1], "status = ?")
	// Inspect the actual shared claim predicate's MySQL rendering as well.
	statement := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return nativeWaitStillValid(tx.Model(&NativeTask{}), time.Minute).Where("id = ?", "task").UpdateColumn("status", NativeStatusDispatching)
	})
	require.True(t, strings.Contains(statement, "TIMESTAMPADD(MICROSECOND, -60000000, UTC_TIMESTAMP(6))"), statement)
}

func TestMySQLNativeDispatchLockErrorRollsBackWithoutTransition(t *testing.T) {
	failure := errors.New("synthetic row lock failure")
	record := &queueSQLRecorder{lockErr: failure}
	db := queueMySQLRecordingDB(t, record)
	require.ErrorIs(t, StartNativeDispatchSubmission(db, "task", "group", 1, "owner", time.Now()), failure)
	require.Equal(t, []string{"begin", "query", "rollback"}, record.events)
}
