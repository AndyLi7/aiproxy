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

// native-task-v2 upgrades a table created by the v1 script deployed before
// public_error existed: storage is refused before it and ready after it, and
// existing rows keep their data. The v1 script regenerated from the model
// already has the column, so a new database never needs v2.
func TestNativeV2AddsPublicErrorToAnEarlierV1Table(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres", "mysql"} {
		saved, err := os.ReadFile(filepath.Join("..", "..", "migrations", "native-task-v2", dialect+".sql"))
		require.NoError(t, err)
		sql := string(saved)
		require.Contains(t, sql, "REVIEW REQUIRED")
		require.Contains(t, sql, "ADD COLUMN")
		require.Contains(t, sql, "public_error")
		require.NotContains(t, sql, "DROP ")
		require.NotContains(t, sql, "NOT NULL")
		if dialect == "mysql" {
			require.Contains(t, sql, "longtext")
		}
		if dialect == "postgres" {
			require.Contains(t, sql, "IF NOT EXISTS")
			require.Contains(t, sql, "lock_timeout")
			require.Contains(t, sql, "COMMIT;")
		}
	}
	current, err := plan("sqlite")
	require.NoError(t, err)
	earlier := strings.Replace(current, ",`public_error` text", "", 1)
	require.NotEqual(t, current, earlier)
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "upgrade.db"))
	require.NoError(t, err)
	for _, statement := range strings.Split(earlier, ";") {
		if strings.TrimSpace(statement) != "" {
			require.NoError(t, db.Exec(statement).Error)
		}
	}
	require.NoError(t, db.Exec("INSERT INTO native_tasks (id, group_id, token_id, model, fingerprint, output_schema, output_schema_hash, status, error_code) VALUES ('kept', 'g', 1, 'a/b/c', 'fp', '{}', 'h', 'failed', 'upstream_rejected')").Error)
	require.False(t, model.NativeTaskStorageReady(db), "a binary ahead of the schema must refuse native storage")
	upgrade, err := os.ReadFile(filepath.Join("..", "..", "migrations", "native-task-v2", "sqlite.sql"))
	require.NoError(t, err)
	require.NoError(t, db.Exec(string(upgrade)).Error)
	require.True(t, model.NativeTaskStorageReady(db))
	task, err := model.GetNativeTask(db, "kept", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "upstream_rejected", task.ErrorCode)
	require.Empty(t, task.PublicError)
	require.Error(t, db.Exec(string(upgrade)).Error) // A repeated apply fails and changes nothing.
}
