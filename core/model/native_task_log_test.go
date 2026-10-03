package model_test

import (
	"path/filepath"
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const nativeLogSchema = `{"type":"object","required":["images"],"properties":{"images":{"type":"array"}}}`

func nativeLogDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "native-log.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}, &model.Log{}, &model.RequestDetail{}))
	return db
}

func submittedNativeTask(t *testing.T, db *gorm.DB, id string) *model.NativeTask {
	t.Helper()
	task, created, err := model.ReserveNativeTask(db, model.NativeTask{
		ID: id, GroupID: "g", TokenID: 1, Model: "alibaba/wan-2.2-5b/text-to-image",
		Fingerprint: "hash", OutputSchema: nativeLogSchema, ChannelID: 7,
		PrepaymentQuoteJSON: `{"version":1,"currency":"USD","quoteVersion":"q1","prepaidMicros":200,"routes":[{"routeId":"r","channelId":7,"provider":"fal","endpoint":"fal-ai/x","credentialScope":"s","quantityMetric":"request","estimatedMicros":100,"prepaidMicros":200,"rule":{"mode":"cost_markup","ratio":"1"}}]}`,
	})
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, model.TransitionNativeSubmission(db, id, "g", 1, "reserved", "submitting", ""))
	return task
}

func nativeLogRows(t *testing.T, db *gorm.DB, id string) []model.Log {
	t.Helper()
	var rows []model.Log
	require.NoError(t, db.Where("request_id = ?", id).Order("id").Find(&rows).Error)
	return rows
}

func TestNativeTaskLogFollowsTaskToCompletionAndCharge(t *testing.T) {
	db := nativeLogDB(t)
	task := submittedNativeTask(t, db, "pg_native_log")
	info := model.NativeTaskLog{TokenName: "playground", Endpoint: "POST /v1/model-tasks", RequestSource: model.RequestSourcePlayground, IP: "127.0.0.1", Mode: int(mode.NativeTasks)}
	require.NoError(t, model.RecordNativeTaskLog(db, task, info))
	// A replayed submission never adds a second row.
	require.NoError(t, model.RecordNativeTaskLog(db, task, info))
	// A gateway rejection for the same request id is a separate row and stays untouched.
	require.NoError(t, db.Create(&model.Log{RequestID: "pg_native_log", GroupID: "g", TokenID: 1, Code: 503, Endpoint: "POST /v1/model-tasks"}).Error)
	// So is an async image task of the same key that reused the request id.
	require.NoError(t, db.Create(&model.Log{RequestID: "pg_native_log", GroupID: "g", TokenID: 1, Code: 202, Mode: int(mode.ImagesGenerations), AsyncUsageStatus: model.AsyncUsageStatusPending}).Error)

	rows := nativeLogRows(t, db, "pg_native_log")
	require.Len(t, rows, 3)
	require.Equal(t, "alibaba/wan-2.2-5b", rows[0].Model)
	require.Equal(t, "text-to-image", rows[0].Capability)
	require.Equal(t, 202, rows[0].Code)
	require.Equal(t, 7, rows[0].ChannelID)
	require.Equal(t, model.RequestSourcePlayground, rows[0].RequestSource)
	require.Equal(t, model.AsyncUsageStatusPending, rows[0].AsyncUsageStatus)
	// Explicit USD pricing lets customer pages show the charged amount.
	require.Equal(t, "USD", rows[0].Currency)
	require.Equal(t, "native:q1", rows[0].PricingVersion)

	require.NoError(t, model.AcceptNativeTask(db, "pg_native_log", "g", 1, "fal-request-1"))
	require.NoError(t, model.SaveNativeBillingTerminal(db, "pg_native_log", "g", 1, `{"status":"accepted"}`))
	rows = nativeLogRows(t, db, "pg_native_log")
	require.Equal(t, "fal-request-1", string(rows[0].UpstreamID))
	require.Equal(t, model.AsyncUsageStatusPending, rows[0].AsyncUsageStatus)

	require.NoError(t, db.Model(&model.NativeTask{}).Where("id = ?", "pg_native_log").Update("status", "completed").Error)
	require.NoError(t, model.SaveNativeBillingTerminal(db, "pg_native_log", "g", 1, `{"status":"settled","chargedMicros":19200}`))
	rows = nativeLogRows(t, db, "pg_native_log")
	require.Equal(t, model.AsyncUsageStatusCompleted, rows[0].AsyncUsageStatus)
	require.InDelta(t, 0.0192, rows[0].Amount.UsedAmount, 1e-9)
	require.Equal(t, 503, rows[1].Code)
	require.Equal(t, model.AsyncUsageStatusNone, rows[1].AsyncUsageStatus)
	require.Equal(t, 202, rows[2].Code)
	require.Equal(t, model.AsyncUsageStatusPending, rows[2].AsyncUsageStatus)
	require.Empty(t, string(rows[2].UpstreamID))
	require.Zero(t, rows[2].Amount.UsedAmount)
}

func TestNativeTaskLogMarksFailedTasks(t *testing.T) {
	db := nativeLogDB(t)
	task := submittedNativeTask(t, db, "trial_native_failed")
	require.NoError(t, model.RecordNativeTaskLog(db, task, model.NativeTaskLog{Endpoint: "POST /api/native-trials", RequestSource: model.RequestSourceAdminDemo, Mode: int(mode.NativeTasks)}))
	require.NoError(t, model.TransitionNativeSubmission(db, "trial_native_failed", "g", 1, "submitting", "failed", "upstream_rejected"))
	require.NoError(t, model.SaveNativeBillingTerminal(db, "trial_native_failed", "g", 1, `{"status":"refunded","chargedMicros":0}`))
	rows := nativeLogRows(t, db, "trial_native_failed")
	require.Len(t, rows, 1)
	require.Equal(t, 502, rows[0].Code)
	require.Equal(t, model.AsyncUsageStatusFailed, rows[0].AsyncUsageStatus)
	require.Equal(t, "upstream_rejected", rows[0].ErrorCode)
	require.Equal(t, model.RequestSourceAdminDemo, rows[0].RequestSource)
	require.Zero(t, rows[0].Amount.UsedAmount)
}

func TestNativeTaskLogIsSkippedWithoutLogStorage(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "native-only.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	task, _, err := model.ReserveNativeTask(db, model.NativeTask{ID: "no_logs", GroupID: "g", TokenID: 1, Model: "a/b/c", Fingerprint: "hash", OutputSchema: nativeLogSchema})
	require.NoError(t, err)
	require.NoError(t, model.RecordNativeTaskLog(db, task, model.NativeTaskLog{}))
	require.NoError(t, model.SaveNativeBillingTerminal(db, "no_logs", "g", 1, `{"status":"accepted"}`))
}
