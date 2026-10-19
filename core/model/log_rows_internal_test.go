package model

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

var errIPGroupsRead = errors.New("synthetic IP group iteration failure")

type ipGroupsConnector struct{}

func (ipGroupsConnector) Driver() driver.Driver { return queueSQLDriver{} }
func (ipGroupsConnector) Connect(context.Context) (driver.Conn, error) {
	return &ipGroupsConn{queueSQLConn: queueSQLConn{&queueSQLRecorder{}}}, nil
}

type ipGroupsConn struct{ queueSQLConn }

func (*ipGroupsConn) QueryContext(
	context.Context,
	string,
	[]driver.NamedValue,
) (driver.Rows, error) {
	return &ipGroupsRows{}, nil
}

type ipGroupsRows struct{ delivered bool }

func (*ipGroupsRows) Columns() []string { return []string{"ip", "groups"} }
func (*ipGroupsRows) Close() error      { return nil }
func (r *ipGroupsRows) Next(values []driver.Value) error {
	if r.delivered {
		return errIPGroupsRead
	}

	values[0], values[1] = "192.0.2.1", "group-a,group-b"
	r.delivered = true

	return nil
}

func TestGetIPGroupsRejectsPartialIteration(t *testing.T) {
	pool := sql.OpenDB(ipGroupsConnector{})
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})

	db, err := gorm.Open(
		mysql.New(mysql.Config{Conn: pool, SkipInitializeWithVersion: true}),
		&gorm.Config{},
	)
	if err != nil {
		t.Fatal(err)
	}

	original := LogDB
	LogDB = db
	t.Cleanup(func() { LogDB = original })

	groups, err := GetIPGroups(1, time.Time{}, time.Time{})
	if !errors.Is(err, errIPGroupsRead) || groups != nil {
		t.Fatalf("partial iteration returned success: %v %v", groups, err)
	}
}
