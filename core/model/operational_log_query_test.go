package model

import (
	"fmt"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

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
