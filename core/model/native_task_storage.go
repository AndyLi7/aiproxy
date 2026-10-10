package model

import (
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// NativeTaskStorageReady checks every persisted field, not just table existence.
// An empty projection reads no task data and performs no migration. This also
// detects a partially applied or older migration before advertising execution.
func NativeTaskStorageReady(db *gorm.DB) bool {
	if db == nil {
		return false
	}
	statement := &gorm.Statement{DB: db}
	if err := statement.Parse(&NativeTask{}); err != nil {
		return false
	}
	rows, err := db.Session(&gorm.Session{Logger: logger.Discard}).Model(&NativeTask{}).
		Select(statement.Schema.DBNames).Limit(0).Rows()
	if err != nil {
		return false
	}
	defer rows.Close()
	return rows.Err() == nil
}
