package main

import (
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeDDLIsAdditiveAndSQLiteExecutionPreservesExistingData(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres", "mysql"} {
		sql, err := plan(dialect)
		require.NoError(t, err)
		saved, err := os.ReadFile(filepath.Join("..", "..", "migrations", "native-task-v1", dialect+".sql"))
		require.NoError(t, err)
		require.Equal(t, string(saved), sql, "reviewed migration must match current model")
		require.Contains(t, sql, "native_tasks")
		require.Contains(t, sql, "PRIMARY KEY")
		require.NotContains(t, sql, "DROP ")
		require.NotContains(t, sql, "ALTER ")
		if dialect == "mysql" {
			require.Contains(t, sql, "longtext")
			require.NotContains(t, sql, " text")
		}
	}
	sql, err := plan("sqlite")
	require.NoError(t, err)
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "migration.db"))
	require.NoError(t, err)
	require.NoError(t, db.Exec("CREATE TABLE existing_data (id INTEGER PRIMARY KEY, value TEXT)").Error)
	require.NoError(t, db.Exec("INSERT INTO existing_data VALUES (1, 'keep')").Error)
	for _, statement := range strings.Split(sql, ";") {
		if strings.TrimSpace(statement) != "" {
			require.NoError(t, db.Exec(statement).Error)
		}
	}
	require.True(t, db.Migrator().HasTable(&model.NativeTask{}))
	require.True(t, model.NativeTaskStorageReady(db))
	task, created, err := model.ReserveNativeTask(db, model.NativeTask{ID: "request", GroupID: "owner", TokenID: 1, Model: "vendor/model/native", Fingerprint: "fp", OutputSchema: `{}`})
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "reserved", task.Status)
	var value string
	require.NoError(t, db.Raw("SELECT value FROM existing_data WHERE id = 1").Scan(&value).Error)
	require.Equal(t, "keep", value)
	require.Error(t, db.Exec(strings.Split(sql, ";")[0]).Error) // A repeated apply must not mask an existing incompatible table.
}
