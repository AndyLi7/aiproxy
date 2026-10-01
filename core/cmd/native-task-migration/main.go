// Print reviewable additive DDL only. Never reads environment DSNs or applies SQL.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/labring/aiproxy/core/model"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type ddlLog struct {
	logger.Interface
	statements []string
}

func (l *ddlLog) Trace(_ context.Context, _ time.Time, fc func() (string, int64), err error) {
	if err == nil {
		sql, _ := fc()
		l.statements = append(l.statements, sql)
	}
}
func plan(dialect string) (string, error) {
	var driver gorm.Dialector
	switch dialect {
	case "sqlite":
		driver = sqlite.Open(":memory:")
	case "postgres":
		driver = postgres.New(postgres.Config{DSN: "host=127.0.0.1 port=1 user=ddl dbname=ddl sslmode=disable"})
	case "mysql":
		driver = mysql.New(mysql.Config{DSN: "ddl@tcp(127.0.0.1:1)/ddl", SkipInitializeWithVersion: true})
	default:
		return "", fmt.Errorf("unsupported dialect")
	}
	capture := &ddlLog{Interface: logger.Discard}
	db, err := gorm.Open(driver, &gorm.Config{DryRun: true, DisableAutomaticPing: true, Logger: capture})
	if err != nil {
		return "", err
	}
	conn, err := db.DB()
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err = db.Migrator().CreateTable(&model.NativeTask{}); err != nil {
		return "", err
	}
	for _, statement := range capture.statements {
		if !strings.HasPrefix(statement, "CREATE TABLE ") && !strings.HasPrefix(statement, "CREATE INDEX ") {
			return "", fmt.Errorf("unexpected non-additive DDL")
		}
	}
	if len(capture.statements) == 0 {
		return "", fmt.Errorf("empty DDL")
	}
	// Native JSON is bounded by the runtime but can exceed MySQL TEXT's 64 KiB.
	// PostgreSQL/SQLite text already accommodates that runtime limit.
	if dialect == "mysql" {
		for i := range capture.statements {
			capture.statements[i] = strings.ReplaceAll(capture.statements[i], " text", " longtext")
		}
	}
	return "-- Review before applying to the gateway LOG database. Not an automatic migration.\n" + strings.Join(capture.statements, ";\n") + ";\n", nil
}
func main() {
	dialect := flag.String("dialect", "", "required: sqlite, postgres or mysql")
	flag.Parse()
	sql, err := plan(*dialect)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(sql)
}
