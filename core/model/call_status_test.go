//nolint:testpackage // The SQL renderers of the classifier are internal.
package model

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// A new relay mode without a class fails here, so it can never be counted by
// accident. The table may not keep entries for modes that no longer exist.
func TestEveryRelayModeIsClassified(t *testing.T) {
	all := mode.All()
	for _, relayMode := range all {
		if _, ok := relayModeClasses[relayMode]; !ok {
			t.Fatalf("relay mode %s (%d) has no call class", relayMode, relayMode)
		}
	}

	require.Len(t, relayModeClasses, len(all), "the class table lists a mode that does not exist")

	for relayMode := range asyncUsageBillingOnlyModes {
		require.Equal(t, modeClassSyncCall, relayModeClasses[relayMode],
			"billing-only async usage applies to sync-call modes only: %s", relayMode)
	}
}

func TestCallStatusRules(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Hour)
	stale := now.Add(-25 * time.Hour)

	for _, tc := range []struct {
		name string
		log  Log
		want CallStatus
	}{
		// Old rule: processing. The async status of a sync row is the
		// measured-image billing outbox, not the call's outcome.
		{"sync 200 / async 1", Log{Mode: int(mode.ImagesGenerations), Code: 200, AsyncUsageStatus: AsyncUsageStatusPending, RequestAt: fresh}, CallStatusSucceeded},
		{"sync 200 / async 4", Log{Mode: int(mode.ImagesGenerations), Code: 200, AsyncUsageStatus: AsyncUsageStatusMeasurementPending, RequestAt: stale}, CallStatusSucceeded},
		// Old rule: failed for every 202 task row.
		{"202 / async 1, fresh", Log{Mode: int(mode.NativeTasks), Code: 202, AsyncUsageStatus: AsyncUsageStatusPending, RequestAt: fresh}, CallStatusProcessing},
		{"202 / async 1, 25 h old", Log{Mode: int(mode.NativeTasks), Code: 202, AsyncUsageStatus: AsyncUsageStatusPending, RequestAt: stale}, CallStatusUnknown},
		{"202 / async 2", Log{Mode: int(mode.NativeTasks), Code: 202, AsyncUsageStatus: AsyncUsageStatusCompleted, RequestAt: stale}, CallStatusSucceeded},
		{"202 / async 3", Log{Mode: int(mode.NativeTasks), Code: 202, AsyncUsageStatus: AsyncUsageStatusFailed, RequestAt: fresh}, CallStatusFailed},
		{"video 200 / async 1", Log{Mode: int(mode.Videos), Code: 200, AsyncUsageStatus: AsyncUsageStatusPending, RequestAt: fresh}, CallStatusProcessing},
		{"video 200 / async 3", Log{Mode: int(mode.Videos), Code: 200, AsyncUsageStatus: AsyncUsageStatusFailed, RequestAt: fresh}, CallStatusFailed},
		{"background response 200 / async 1", Log{Mode: int(mode.Responses), Code: 200, AsyncUsageStatus: AsyncUsageStatusPending, RequestAt: fresh}, CallStatusProcessing},
		{"plain response 200 / async 0", Log{Mode: int(mode.Responses), Code: 200, RequestAt: stale}, CallStatusSucceeded},
		{"402 balance", Log{Mode: int(mode.ChatCompletions), Code: 402, FailureStage: FailureStageBalance}, CallStatusRejected},
		{"429 rate_limit", Log{Mode: int(mode.ChatCompletions), Code: 429, FailureStage: FailureStageRateLimit}, CallStatusRejected},
		{"502 upstream", Log{Mode: int(mode.ChatCompletions), Code: 502, FailureStage: FailureStageUpstream}, CallStatusFailed},
		{"503 with stage ''", Log{Mode: int(mode.ImagesGenerations), Code: 503}, CallStatusFailed},
		// Deliberate (plan §7): a prepaid image task reaches async 2 only when
		// the wallet reports the charge, even after delivery.
		{"prepaid image task delivered, bill not yet charged", Log{Mode: int(mode.ImagesGenerations), Code: 202, AsyncUsageStatus: AsyncUsageStatusPending, RequestAt: stale}, CallStatusUnknown},
		{"TokenAuth rejection: 403, stage auth, token 7", Log{Code: 403, FailureStage: FailureStageAuth, TokenID: 7, GroupID: "g"}, CallStatusRejected},
		{"native task failed after acceptance", Log{Mode: int(mode.NativeTasks), Code: 502, AsyncUsageStatus: AsyncUsageStatusFailed}, CallStatusFailed},
		// Key usage P2: a row with an op is one where money may be held, so a
		// gate stage alone no longer makes it a rejection. Old: rejected.
		{"503 routing with op", Log{Mode: int(mode.NativeTasks), Code: 503, FailureStage: FailureStageRouting, BillingOperationID: "native:t"}, CallStatusFailed},
		{"409 validation with op (lost reserved->submitting race)", Log{Mode: int(mode.NativeTasks), Code: 409, FailureStage: FailureStageValidation, BillingOperationID: "native:t"}, CallStatusFailed},
		{"402 balance without op", Log{Mode: int(mode.NativeTasks), Code: 402, FailureStage: FailureStageBalance}, CallStatusRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.log.CallStatus(now))
		})
	}
}

func TestIsCall(t *testing.T) {
	call := Log{GroupID: "g", TokenID: 7, Mode: int(mode.ChatCompletions), Endpoint: "/v1/chat/completions"}
	require.True(t, call.IsCall())

	for _, tc := range []struct {
		name   string
		mutate func(*Log)
		want   bool
	}{
		{"native task submit", func(l *Log) { l.Mode, l.Endpoint = int(mode.NativeTasks), "POST /v1/model-tasks" }, true},
		{"image task submit", func(l *Log) { l.Mode, l.Endpoint = int(mode.ImagesGenerations), "/v1/images/tasks" }, true},
		{"post-auth rejection on a POST", func(l *Log) { l.Mode, l.Endpoint, l.Code = 0, "POST /v1/chat/completions", 403 }, true},
		{"playground", func(l *Log) { l.RequestSource = RequestSourcePlayground }, true},
		{"historical unknown mode", func(l *Log) { l.Mode = 999 }, true},
		{"key not recognised", func(l *Log) { l.TokenID = 0 }, false},
		{"no group", func(l *Log) { l.GroupID = "" }, false},
		{"admin demo", func(l *Log) { l.RequestSource = RequestSourceAdminDemo }, false},
		{"relay read mode", func(l *Log) { l.Mode, l.Code = int(mode.VideosGet), 502 }, false},
		{"alpha search", func(l *Log) { l.Mode = int(mode.AlphaSearch) }, false},
		{"failed poll", func(l *Log) { l.Mode, l.Endpoint, l.Code = 0, "GET /v1/model-tasks/x", 404 }, false},
		{"failed delete", func(l *Log) { l.Mode, l.Endpoint, l.Code = 0, "DELETE /v1/videos/x", 404 }, false},
		{"models list", func(l *Log) { l.Mode, l.Endpoint, l.Code = 0, "GET /v1/models", 401 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := call
			tc.mutate(&entry)
			require.Equal(t, tc.want, entry.IsCall())
		})
	}
}

func TestCallStatusSQLMatchesGo(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		db, err := OpenSQLite(filepath.Join(t.TempDir(), "call-status.db"))
		require.NoError(t, err)
		t.Cleanup(func() {
			if sqlDB, err := db.DB(); err == nil {
				_ = sqlDB.Close()
			}
		})

		assertCallStatusSQLMatchesGo(t, db)
	})

	t.Run("postgres", func(t *testing.T) {
		withTestPostgresStoreDB(t, func() {
			assertCallStatusSQLMatchesGo(t, LogDB)
		})
	})
}

func assertCallStatusSQLMatchesGo(t *testing.T, db *gorm.DB) {
	t.Helper()

	require.NoError(t, db.AutoMigrate(&Log{}))

	now := time.Now()
	requestTimes := []time.Time{now.Add(-time.Hour), now.Add(-25 * time.Hour)}
	modes := []int{
		int(mode.ChatCompletions),
		int(mode.ImagesGenerations),
		int(mode.Videos),
		int(mode.VideosGet),
		int(mode.NativeTasks),
	}
	endpoints := []EmptyNullString{"", "/v1/images/generations", "POST /v1/model-tasks", "GET /v1/model-tasks/x"}
	stages := []FailureStage{
		FailureStageNone,
		FailureStageUpstream,
		FailureStageValidation,
		FailureStageAuth,
		FailureStageBalance,
	}

	rows := make([]Log, 0, 112000)
	for _, code := range []int{200, 202, 400, 402, 429, 502, 503} {
		for async := AsyncUsageStatusNone; async <= AsyncUsageStatusMeasurementPending; async++ {
			for _, stage := range stages {
				for _, relayMode := range modes {
					for _, endpoint := range endpoints {
						for _, requestAt := range requestTimes {
							for _, source := range []string{RequestSourceAPI, RequestSourceAdminDemo} {
								for _, tokenID := range []int{0, 7} {
									for _, op := range []EmptyNullString{"", "op"} {
										rows = append(rows, Log{
											GroupID:            "g",
											TokenID:            tokenID,
											Code:               code,
											AsyncUsageStatus:   async,
											FailureStage:       stage,
											Mode:               relayMode,
											Endpoint:           endpoint,
											RequestAt:          requestAt,
											CreatedAt:          requestAt,
											RequestSource:      source,
											BillingOperationID: op,
										})
									}
								}
							}
						}
					}
				}
			}
		}
	}

	require.NoError(t, db.CreateInBatches(&rows, 300).Error)

	// NULL columns from historical rows must classify like their zero values.
	for _, column := range []string{"mode", "group_id", "token_id", "request_source", "failure_stage", "endpoint", "billing_operation_id"} {
		values := map[string]any{
			"group_id":           "g",
			"token_id":           7,
			"code":               202,
			"async_usage_status": int(AsyncUsageStatusPending),
			"mode":               int(mode.NativeTasks),
			"endpoint":           "POST /v1/model-tasks",
			"request_source":     RequestSourceAPI,
			"failure_stage":      "",
			"request_at":         now,
			"created_at":         now,
			// A gate rejection with an op: NULL op must classify as no op.
			"billing_operation_id": "op",
		}
		if column == "billing_operation_id" {
			values["code"] = 402
			values["failure_stage"] = string(FailureStageBalance)
		}
		values[column] = nil
		require.NoError(t, db.Table("logs").Create(values).Error, column)
	}

	statusSQL, statusArgs := callStatusExpr(now)
	callSQL, callArgs := isCallExpr()

	args := make([]any, 0, len(statusArgs)+len(callArgs))
	args = append(args, statusArgs...)
	args = append(args, callArgs...)

	var classified []struct {
		ID     int
		Status string
		IsCall int
	}
	require.NoError(t, db.Model(&Log{}).
		Select("id, ("+statusSQL+") AS status, CASE WHEN "+callSQL+" THEN 1 ELSE 0 END AS is_call", args...).
		Find(&classified).Error)

	var stored []Log
	require.NoError(t, db.Find(&stored).Error)
	require.Len(t, classified, len(stored))

	byID := make(map[int]*Log, len(stored))
	for index := range stored {
		byID[stored[index].ID] = &stored[index]
	}

	counts := map[string]int{}

	for _, row := range classified {
		entry := byID[row.ID]
		require.NotNil(t, entry)

		description := fmt.Sprintf("%+v", *entry)
		require.Equal(t, string(entry.CallStatus(now)), row.Status, description)
		require.Equal(t, entry.IsCall(), row.IsCall == 1, description)
		counts[row.Status]++
	}

	for _, status := range CallStatuses {
		require.Positive(t, counts[string(status)], "fixture covers %s", status)
	}

	// The status filter of the list endpoints uses the same CASE.
	for _, status := range CallStatuses {
		var filtered int64
		require.NoError(t, applyOperationalLogFilter(
			db.Model(&Log{}),
			OperationalLogFilter{Status: status, Now: now},
		).Count(&filtered).Error)
		require.EqualValues(t, counts[string(status)], filtered, status)
	}
}
