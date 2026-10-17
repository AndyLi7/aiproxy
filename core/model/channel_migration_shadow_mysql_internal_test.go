//nolint:testpackage // Real GORM/MySQL SQL recording, not a MySQL server/locking test.
package model

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type shadowMySQLConnector struct {
	record  *queueSQLRecorder
	present int64
}

func (c shadowMySQLConnector) Driver() driver.Driver { return queueSQLDriver{} }
func (c shadowMySQLConnector) Connect(context.Context) (driver.Conn, error) {
	return &shadowMySQLConn{queueSQLConn: queueSQLConn{record: c.record}, present: c.present}, nil
}

type shadowMySQLConn struct {
	queueSQLConn
	present int64
}
type shadowMySQLRows struct {
	value *int64
	read  bool
}

func (r *shadowMySQLRows) Columns() []string { return []string{"count"} }
func (r *shadowMySQLRows) Close() error      { return nil }
func (r *shadowMySQLRows) Next(values []driver.Value) error {
	if r.value == nil || r.read {
		return io.EOF
	}
	r.read = true
	values[0] = *r.value
	return nil
}
func (c *shadowMySQLConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.record.events = append(c.record.events, "query")
	c.record.queries = append(c.record.queries, q)
	if strings.Contains(q, "information_schema.tables") {
		n := int64(0)
		for _, v := range args {
			if v.Value == "channel_migration_shadow_protocols" || v.Value == "channel_migration_shadow_resources" {
				n = c.present
			}
		}
		return &shadowMySQLRows{value: &n}, nil
	}
	return &shadowMySQLRows{}, nil
}
func TestMigrationShadowMySQLLegacyOrdinaryEntryCompatibility(t *testing.T) {
	for _, present := range []int64{0, 1, 2} {
		for _, action := range []string{"update", "delete", "create", "status"} {
			t.Run(action+string(rune('0'+present)), func(t *testing.T) {
				record := &queueSQLRecorder{}
				pool := sql.OpenDB(shadowMySQLConnector{record, present})
				defer pool.Close()
				db, e := gorm.Open(mysql.New(mysql.Config{Conn: pool, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
				require.NoError(t, e)
				oldDB, oldLog := DB, LogDB
				DB, LogDB = db, db
				defer func() { DB, LogDB = oldDB, oldLog }()
				switch action {
				case "update":
					name := "ordinary-edit"
					e = UpdateChannel(&Channel{ID: 7}, &ChannelPatch{Name: &name})
				case "delete":
					e = DeleteChannelByID(7)
				case "create":
					e = BatchInsertChannels([]*Channel{{Name: "ordinary-create", Status: ChannelStatusDisabled}})
				case "status":
					e = UpdateChannelStatusByID(7, ChannelStatusDisabled)
				}
				if present == 0 {
					require.NoError(t, e)
					require.Contains(t, record.events, "update")
				} else {
					require.Error(t, e)
					require.NotContains(t, record.events, "update")
				}
				require.Contains(t, strings.Join(record.queries, "\n"), "table_schema=DATABASE()")
			})
		}
	}
}

type shadowMySQLResult struct{}

func (shadowMySQLResult) LastInsertId() (int64, error) { return 99, nil }
func (shadowMySQLResult) RowsAffected() (int64, error) { return 1, nil }
func (c *shadowMySQLConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	_, e := c.queueSQLConn.ExecContext(ctx, q, args)
	return shadowMySQLResult{}, e
}
