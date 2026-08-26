package model

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestGetLogsExcludesModesFromRowsAndTotal(t *testing.T) {
	database, err := OpenSQLite(filepath.Join(t.TempDir(), "group-logs.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	previousLogDB := LogDB
	LogDB = database
	t.Cleanup(func() {
		LogDB = previousLogDB
		sqlDB, sqlErr := database.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})

	if err := database.AutoMigrate(&Log{}, &RequestDetail{}); err != nil {
		t.Fatalf("migrate log database: %v", err)
	}

	createdAt := time.Unix(1_787_083_200, 0)
	logs := []Log{
		{GroupID: "group-a", RequestID: "req-create", Model: "seedance", Mode: 22, Code: 200, CreatedAt: createdAt},
		{GroupID: "group-a", RequestID: "req-poll", Mode: 37, Code: 500, CreatedAt: createdAt.Add(time.Second)},
		{GroupID: "group-b", RequestID: "req-other-tenant", Model: "seedance", Mode: 22, Code: 200, CreatedAt: createdAt.Add(2 * time.Second)},
	}
	if err := database.Create(&logs).Error; err != nil {
		t.Fatalf("seed logs: %v", err)
	}
	if err := database.Table("logs").Create(map[string]any{
		"group_id":   "group-a",
		"request_id": "req-legacy",
		"model":      "legacy-model",
		"mode":       nil,
		"code":       200,
		"created_at": createdAt.Add(3 * time.Second),
		"request_at": createdAt.Add(3 * time.Second),
	}).Error; err != nil {
		t.Fatalf("seed legacy log: %v", err)
	}

	total, got, err := getLogs(
		"group-a",
		time.Time{},
		time.Time{},
		"",
		"",
		"",
		0,
		"",
		0,
		"id-desc",
		CodeTypeAll,
		0,
		false,
		"",
		"",
		OperationalLogFilter{ExcludedModes: []int{37}},
		1,
		20,
	)
	if err != nil {
		t.Fatalf("get filtered logs: %v", err)
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}
	if len(got) != 2 || got[0].RequestID.String() != "req-legacy" || got[1].RequestID.String() != "req-create" {
		t.Fatalf("logs = %+v, want req-legacy and req-create", got)
	}
}

func newOperationalLogDryRunDB(t *testing.T) *gorm.DB {
	t.Helper()

	database, err := gorm.Open(postgres.New(postgres.Config{
		DSN: "host=localhost user=test dbname=test sslmode=disable",
	}), &gorm.Config{
		DryRun:               true,
		DisableAutomaticPing: true,
	})
	if err != nil {
		t.Fatalf("open dry-run database: %v", err)
	}

	return database
}

func TestApplyOperationalLogFilterBuildsStatusPredicates(t *testing.T) {
	database := newOperationalLogDryRunDB(t)
	tests := []struct {
		name       string
		status     OperationalStatus
		contains   []string
		wantValues []string
	}{
		{
			name:       "processing",
			status:     OperationalStatusProcessing,
			contains:   []string{"code = 200", "async_usage_status ="},
			wantValues: []string{fmt.Sprint(AsyncUsageStatusPending)},
		},
		{
			name:       "success",
			status:     OperationalStatusSuccess,
			contains:   []string{"code = 200", "async_usage_status !="},
			wantValues: []string{fmt.Sprint(AsyncUsageStatusPending), fmt.Sprint(AsyncUsageStatusFailed)},
		},
		{
			name:       "rejected",
			status:     OperationalStatusRejected,
			contains:   []string{"code != 200", "failure_stage != ''"},
			wantValues: []string{string(FailureStageUpstream)},
		},
		{
			name:       "failed",
			status:     OperationalStatusFailed,
			contains:   []string{"async_usage_status =", "code != 200", "failure_stage = ''"},
			wantValues: []string{fmt.Sprint(AsyncUsageStatusFailed), string(FailureStageUpstream)},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			statement := applyOperationalLogFilter(
				database.Model(&Log{}),
				OperationalLogFilter{Status: test.status},
			).Find(&[]Log{}).Statement

			sql := statement.SQL.String()
			for _, fragment := range test.contains {
				if !strings.Contains(sql, fragment) {
					t.Fatalf("status %q SQL %q does not contain %q", test.status, sql, fragment)
				}
			}

			values := make([]string, 0, len(statement.Vars))
			for _, value := range statement.Vars {
				values = append(values, fmt.Sprint(value))
			}
			if fmt.Sprint(values) != fmt.Sprint(test.wantValues) {
				t.Fatalf("status %q vars %v, want %v", test.status, values, test.wantValues)
			}
		})
	}
}

func TestApplyOperationalLogFilterSupportsMultipleChannels(t *testing.T) {
	database := newOperationalLogDryRunDB(t)
	statement := applyOperationalLogFilter(
		database.Model(&Log{}),
		OperationalLogFilter{ChannelIDs: []int{10, 12}},
	).Find(&[]Log{}).Statement

	sql := statement.SQL.String()
	if !strings.Contains(sql, "channel_id IN") {
		t.Fatalf("multi-channel SQL %q does not contain an IN predicate", sql)
	}
	if fmt.Sprint(statement.Vars) != "[10 12]" {
		t.Fatalf("multi-channel vars %v, want [10 12]", statement.Vars)
	}
}
