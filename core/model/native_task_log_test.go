package model_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/nativeresult"
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
		Fingerprint: "hash", OutputSchema: nativeLogSchema, ChannelID: 7, BillingOperationID: "native:" + id,
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
	require.Equal(t, "Native task failed", rows[0].SafeError)
	require.Empty(t, rows[0].FailureStage)
	require.Zero(t, rows[0].Amount.UsedAmount)
}

// Owner decision 2026-10-09: a task failed with invalid_parameters summarizes
// the rejected fields and rules only, bounded to 256 bytes, without a failure
// stage.
func TestNativeTaskLogNamesRejectedParameters(t *testing.T) {
	db := nativeLogDB(t)
	task := submittedNativeTask(t, db, "pg_native_invalid")
	require.NoError(t, model.RecordNativeTaskLog(db, task, model.NativeTaskLog{Endpoint: "POST /v1/model-tasks", Mode: int(mode.NativeTasks)}))
	require.NoError(t, model.AcceptNativeTask(db, "pg_native_invalid", "g", 1, "fal-request-1"))
	issues := []nativeresult.ParameterIssue{{Field: "voice", Rule: "unsupported_value"}, {Field: "image_urls[0]", Rule: "file_size"}}
	require.NoError(t, model.UpdateNativePoll(db, "pg_native_invalid", "g", 1, "failed", "invalid_parameters", issues...))
	require.NoError(t, model.SaveNativeBillingTerminal(db, "pg_native_invalid", "g", 1, `{"status":"refunded","chargedMicros":0}`))
	rows := nativeLogRows(t, db, "pg_native_invalid")
	require.Len(t, rows, 1)
	require.Equal(t, 502, rows[0].Code)
	require.Equal(t, model.AsyncUsageStatusFailed, rows[0].AsyncUsageStatus)
	require.Equal(t, "invalid_parameters", rows[0].ErrorCode)
	require.Equal(t, "Invalid parameters: voice (unsupported_value), image_urls[0] (file_size)", rows[0].SafeError)
	require.Empty(t, rows[0].FailureStage)

	long := submittedNativeTask(t, db, "pg_native_invalid_long")
	require.NoError(t, model.RecordNativeTaskLog(db, long, model.NativeTaskLog{Mode: int(mode.NativeTasks)}))
	many := make([]nativeresult.ParameterIssue, 8)
	for i := range many {
		many[i] = nativeresult.ParameterIssue{Field: "voice_setting.voice_id_" + strings.Repeat("x", 40), Rule: "unsupported_value"}
	}
	require.NoError(t, model.TransitionNativeSubmission(db, "pg_native_invalid_long", "g", 1, "submitting", "failed", "invalid_parameters", many...))
	require.NoError(t, model.SaveNativeBillingTerminal(db, "pg_native_invalid_long", "g", 1, `{"status":"refunded","chargedMicros":0}`))
	rows = nativeLogRows(t, db, "pg_native_invalid_long")
	require.LessOrEqual(t, len(rows[0].SafeError), 256)
	require.True(t, strings.HasPrefix(rows[0].SafeError, "Invalid parameters: voice_setting.voice_id_"))
	require.True(t, strings.HasSuffix(rows[0].SafeError, "(unsupported_value)"))
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

// A task requested by another accepted ID (public_api_id or an alias) is
// still logged under its capability ID, split into model and capability; the
// requested ID is kept as requested_model metadata. Billing reads the split.
func TestNativeTaskLogKeepsCapabilityIdentityAndRequestedModel(t *testing.T) {
	db := nativeLogDB(t)
	task := submittedNativeTask(t, db, "requested_by_public_id")
	require.NoError(t, model.RecordNativeTaskLog(db, task, model.NativeTaskLog{
		Endpoint: "POST /v1/model-tasks", Mode: int(mode.NativeTasks), RequestedModel: "alibaba/wan-2.2-5b",
	}))
	rows := nativeLogRows(t, db, "requested_by_public_id")
	require.Len(t, rows, 1)
	require.Equal(t, "alibaba/wan-2.2-5b", rows[0].Model)
	require.Equal(t, "text-to-image", rows[0].Capability)
	require.Equal(t, map[string]string{"requested_model": "alibaba/wan-2.2-5b"}, rows[0].Metadata)

	// The capability ID itself adds no metadata.
	task = submittedNativeTask(t, db, "requested_by_capability_id")
	require.NoError(t, model.RecordNativeTaskLog(db, task, model.NativeTaskLog{
		Endpoint: "POST /v1/model-tasks", Mode: int(mode.NativeTasks), RequestedModel: task.Model,
	}))
	rows = nativeLogRows(t, db, "requested_by_capability_id")
	require.Len(t, rows, 1)
	require.Empty(t, rows[0].Metadata)
}

// nativeReceipt is a stored wallet receipt carrying the hold's attribution.
func nativeReceipt(status string, usageAt time.Time, source string) string {
	return fmt.Sprintf(`{"id":"x","status":%q,"chargedMicros":0,"refundMicros":200,"usageAtMs":%d,"usageSource":%q}`,
		status, usageAt.UnixMilli(), source)
}

// Plan §1.4 rule 4: a billing sync inserts the call row a submission lost
// (RecordNativeTaskLog's failure is only logged), only for a task some request
// already answered with 202, never twice, and only with the hold's identity.
func TestNativeMissingRowInsert(t *testing.T) {
	usageAt := time.Date(2026, 10, 12, 1, 2, 3, 456_000_000, time.UTC)

	t.Run("a: lost row is inserted by recovery", func(t *testing.T) {
		db := nativeLogDB(t)
		require.NoError(t, db.AutoMigrate(&model.Token{}))
		require.NoError(t, db.Create(&model.Token{ID: 1, Name: "prod", GroupID: "g"}).Error)

		oldDB := model.DB
		model.DB = db
		t.Cleanup(func() { model.DB = oldDB })

		submittedNativeTask(t, db, "lost_row")
		require.NoError(t, model.AcceptNativeTask(db, "lost_row", "g", 1, "fal-1"))
		// Old: 0 rows; the task completed and was charged without a call row.
		require.NoError(t, model.SaveNativeBillingTerminal(db, "lost_row", "g", 1, nativeReceipt("pending", usageAt, model.RequestSourcePlayground)))
		require.NoError(t, model.SaveNativeBillingTerminal(db, "lost_row", "g", 1, nativeReceipt("pending", usageAt, model.RequestSourcePlayground)))

		rows := nativeLogRows(t, db, "lost_row")
		require.Len(t, rows, 1)
		require.Equal(t, usageAt, rows[0].RequestAt.UTC())
		require.Equal(t, usageAt, rows[0].CreatedAt.UTC())
		require.Equal(t, model.RequestSourcePlayground, rows[0].RequestSource)
		require.Equal(t, model.EmptyNullString("native:lost_row"), rows[0].BillingOperationID)
		require.Equal(t, 202, rows[0].Code)
		require.Equal(t, int(mode.NativeTasks), rows[0].Mode)
		require.Equal(t, "POST /v1/model-tasks", string(rows[0].Endpoint))
		require.Equal(t, "prod", rows[0].TokenName)
		require.Equal(t, "alibaba/wan-2.2-5b", rows[0].Model)
		require.Equal(t, "text-to-image", rows[0].Capability)
		require.Equal(t, "fal-1", string(rows[0].UpstreamID))
		require.Equal(t, map[string]string{"inserted_by": "native_recovery"}, rows[0].Metadata)
		require.Equal(t, model.AsyncUsageStatusPending, rows[0].AsyncUsageStatus)

		// The submitting request's own writer then adds nothing.
		task, err := model.GetNativeTask(db, "lost_row", "g", 1)
		require.NoError(t, err)
		require.NoError(t, model.RecordNativeTaskLog(db, task, model.NativeTaskLog{Mode: int(mode.NativeTasks)}))
		require.Len(t, nativeLogRows(t, db, "lost_row"), 1)
	})

	t.Run("b: refunded reservation gains no row", func(t *testing.T) {
		db := nativeLogDB(t)
		_, created, err := model.ReserveNativeTask(db, model.NativeTask{
			ID: "never_sent", GroupID: "g", TokenID: 1, Model: "a/b/c", Fingerprint: "hash",
			OutputSchema: nativeLogSchema, BillingOperationID: "native:never_sent",
		})
		require.NoError(t, err)
		require.True(t, created)

		require.NoError(t, model.SaveNativeBillingTerminal(db, "never_sent", "g", 1, nativeReceipt("refunded", usageAt, model.RequestSourceAPI)))

		task, err := model.GetNativeTask(db, "never_sent", "g", 1)
		require.NoError(t, err)
		require.Equal(t, "failed", task.Status, "the refund flips the reservation to failed first")
		// A naive insert after the flip gives 1 row for a request that never got a 202.
		require.Empty(t, nativeLogRows(t, db, "never_sent"))
	})

	t.Run("c: a prior rejection row with the op does not block the insert", func(t *testing.T) {
		db := nativeLogDB(t)
		submittedNativeTask(t, db, "after_503")
		require.NoError(t, db.Create(&model.Log{
			RequestID: "after_503", GroupID: "g", TokenID: 1, Code: 503, Mode: int(mode.NativeTasks),
			FailureStage: model.FailureStageRouting, BillingOperationID: "native:after_503",
		}).Error)

		require.NoError(t, model.SaveNativeBillingTerminal(db, "after_503", "g", 1, nativeReceipt("pending", usageAt, model.RequestSourceAPI)))

		rows := nativeLogRows(t, db, "after_503")
		require.Len(t, rows, 2)
		require.Equal(t, 503, rows[0].Code)
		require.Equal(t, 202, rows[1].Code)
		require.Equal(t, model.EmptyNullString("native:after_503"), rows[1].BillingOperationID)
	})

	t.Run("d: a receipt without the hold's time inserts nothing", func(t *testing.T) {
		db := nativeLogDB(t)
		submittedNativeTask(t, db, "pre_p2_hold")
		require.NoError(t, model.SaveNativeBillingTerminal(db, "pre_p2_hold", "g", 1, `{"status":"pending"}`))
		require.NoError(t, model.SaveNativeBillingTerminal(db, "pre_p2_hold", "g", 1, `{"status":"pending","usageAtMs":0,"usageSource":"api"}`))
		require.NoError(t, model.SaveNativeBillingTerminal(db, "pre_p2_hold", "g", 1, fmt.Sprintf(`{"status":"pending","usageAtMs":%d,"usageSource":"web"}`, usageAt.UnixMilli())))
		require.Empty(t, nativeLogRows(t, db, "pre_p2_hold"))
	})

	t.Run("admin trial row keeps its endpoint", func(t *testing.T) {
		db := nativeLogDB(t)
		submittedNativeTask(t, db, "trial_lost")
		require.NoError(t, model.SaveNativeBillingTerminal(db, "trial_lost", "g", 1, nativeReceipt("pending", usageAt, model.RequestSourceAdminDemo)))

		rows := nativeLogRows(t, db, "trial_lost")
		require.Len(t, rows, 1)
		require.Equal(t, "POST /api/native-trials", string(rows[0].Endpoint))
		require.Equal(t, model.RequestSourceAdminDemo, rows[0].RequestSource)
	})
}

// The task's own row carries its op from the first write.
func TestNativeTaskLogCarriesOperation(t *testing.T) {
	db := nativeLogDB(t)
	task := submittedNativeTask(t, db, "with_op")
	require.Equal(t, "native:with_op", task.BillingOperationID)
	require.NoError(t, model.RecordNativeTaskLog(db, task, model.NativeTaskLog{Mode: int(mode.NativeTasks)}))

	rows := nativeLogRows(t, db, "with_op")
	require.Len(t, rows, 1)
	require.Equal(t, model.EmptyNullString("native:with_op"), rows[0].BillingOperationID)
}
