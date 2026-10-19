package model_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func installImageProducer(t *testing.T, db *gorm.DB) {
	t.Helper()
	installExecutionJournal(t, db)
	raw, e := os.ReadFile("../migrations/channel-image-producer-v1/" + db.Name() + ".sql")
	require.NoError(t, e)

	var lines []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}

	parts := strings.Split(strings.TrimSpace(strings.Join(lines, "\n")), ";")
	for range 2 {
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			for _, p := range parts[1 : len(parts)-2] {
				if e := tx.Exec(strings.TrimSpace(p)).Error; e != nil {
					return e
				}
			}

			return nil
		}))
	}
}

func producerRows(t *testing.T, db *gorm.DB) []model.ChannelAdmissionImageProducer {
	t.Helper()

	var rows []model.ChannelAdmissionImageProducer
	require.NoError(t, db.Order("attempt_index").Find(&rows).Error)

	return rows
}

func producerFixture(t *testing.T, f *admissionFixture) (*model.ImageTask, *model.AsyncUsageInfo) {
	t.Helper()

	db := f.store.DB
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	identity := f.identities[0]
	task := &model.ImageTask{
		ID:                    "producer",
		GroupID:               "g",
		TokenID:               1,
		Model:                 "image",
		Fingerprint:           strings.Repeat("a", 64),
		BillingOperationID:    "original-op",
		AdmissionIdentityHash: identity.IdentityHash,
		KeyFingerprint:        identity.KeyDigest,
		ChannelType:           model.ChannelTypeFal,
		UpstreamModel:         "original",
		ExpectedImages:        1,
	}

	var channel model.Channel
	require.NoError(t, db.First(&channel, 1).Error)
	task.ChannelType = channel.Type
	usage := &model.AsyncUsageInfo{
		RequestID:          task.ID,
		GroupID:            "g",
		TokenID:            1,
		BillingOperationID: task.BillingOperationID,
		ChannelID:          1,
		BaseURL:            channel.BaseURL,
		Price:              model.Price{PerRequestPrice: .37},
		PricingVersion:     "frozen-v1",
	}
	saved, created, e := model.ReserveImageTask(task, usage)
	require.NoError(t, e)
	require.True(t, created)

	return saved, usage
}

func prepareProducerAttempt(t *testing.T, f *admissionFixture, task *model.ImageTask) {
	t.Helper()
	attempt, e := model.PrepareImageTaskAttempt(t.Context(), task, f.identities[0], 0)
	require.NoError(t, e)

	task.AdmissionAttempt = attempt
	task.Attempts = []model.ImageTaskAttempt{
		{ChannelID: 1, StartedAt: time.Now(), Decision: "in_flight"},
	}

	var channel model.Channel
	require.NoError(t, f.store.DB.First(&channel, 1).Error)
	require.NoError(t, model.SaveImageTaskAttempt(task, 0, 1, channel.BaseURL))
}

func TestImageProducerLifecycle(t *testing.T) {
	for _, scenario := range []string{"queued", "sync", "failed", "unknown", "receipt-failure", "released", "return-failure", "read-copy", "storage-drift", "late", "partial", "version"} {
		t.Run(scenario, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)

			db := f.store.DB
			if scenario != "late" {
				installImageProducer(t, db)
			}

			task, usage := producerFixture(t, f)
			if scenario == "late" {
				installImageProducer(t, db)
			} else {
				require.Len(t, producerRows(t, db), 1)
			}

			if scenario == "released" {
				require.NoError(t, model.ReleaseImageTaskReservationGeneration(task))
				rows := producerRows(t, db)
				require.Equal(t, "active", rows[0].State)
				require.Contains(t, rows[0].ReceiptJSON, "reservation_released")

				var use model.ChannelAdmissionUse
				require.NoError(t, db.First(&use, "id=?", task.AdmissionGeneration).Error)
				require.Equal(t, "open", use.State)
				require.NoError(t, model.RecordImageProducerReturn(t.Context(), task, true))
				require.NoError(t, model.RecordImageProducerReturn(t.Context(), task, true))
				require.NoError(t, db.First(&use, "id=?", task.AdmissionGeneration).Error)
				require.Equal(t, "closed", use.State)
				require.Equal(t, "error_returned", producerRows(t, db)[0].State)

				return
			}

			prepareProducerAttempt(t, f, task)

			if scenario == "late" {
				require.Empty(t, producerRows(t, db))
				require.NoError(t, model.AcceptSubmittedImageTask(t.Context(), task, "upstream"))
				require.NoError(t, model.RecordImageProducerReturn(t.Context(), task, false))
				require.Empty(t, producerRows(t, db))

				return
			}

			require.Len(t, producerRows(t, db), 2)

			if scenario == "receipt-failure" {
				require.NoError(
					t,
					db.Callback().
						Update().
						Before("gorm:update").
						Register("producer:fail-receipt", func(tx *gorm.DB) {
							if tx.Statement.Table == "channel_admission_image_producers" {
								_ = tx.AddError(errors.New("synthetic journal failure"))
							}
						}),
				)
			}

			var e error
			switch scenario {
			case "sync":
				e = model.CompleteSubmittedImageTask(
					t.Context(),
					task,
					[]model.ImageOutput{{URL: "https://example.invalid/output.png"}},
				)
			case "failed":
				e = model.SetSubmittedImageTaskResult(t.Context(), task, "failed", nil)
			case "unknown":
				e = model.SetSubmittedImageTaskResult(t.Context(), task, "submission_unknown", nil)
			default:
				e = model.AcceptSubmittedImageTask(t.Context(), task, "upstream")
			}

			if scenario == "receipt-failure" {
				require.Error(t, e)

				var current model.ImageTask
				require.NoError(t, db.First(&current, "id=?", task.ID).Error)
				require.Equal(t, "submitting", current.Status)

				var u model.AsyncUsageInfo
				require.NoError(t, db.First(&u, usage.ID).Error)
				require.Equal(t, model.AsyncUsageStatusNone, u.Status)
				require.Empty(t, producerRows(t, db)[0].ReceiptJSON)

				return
			}

			require.NoError(t, e)

			before := producerRows(t, db)
			switch scenario {
			case "read-copy":
				fresh, e := model.GetImageTask(task.ID, "g", 1)
				require.NoError(t, e)
				require.NoError(t, model.RecordImageProducerReturn(t.Context(), fresh, false))
				require.Equal(t, before, producerRows(t, db))

				return
			case "return-failure":
				require.NoError(
					t,
					db.Callback().
						Update().
						Before("gorm:update").
						Register("producer:fail-return", func(tx *gorm.DB) {
							if tx.Statement.Table == "channel_admission_image_producers" {
								_ = tx.AddError(errors.New("synthetic return failure"))
							}
						}),
				)
			case "storage-drift":
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionControl{}).
						Where("id=1").
						Update("storage_generation", "different").
						Error,
				)
			case "partial":
				require.NoError(
					t,
					db.Exec(
						"ALTER TABLE channel_admission_image_producers DROP COLUMN receipt_hash",
					).Error,
				)
			case "version":
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionExecutionProtocol{}).
						Where("id=8").
						Update("version", 99).
						Error,
				)
			}

			e = model.RecordImageProducerReturn(context.Background(), task, false)
			if scenario == "return-failure" || scenario == "storage-drift" ||
				scenario == "partial" ||
				scenario == "version" {
				require.Error(t, e)
				return
			}

			require.NoError(t, e)
			require.NoError(t, model.RecordImageProducerReturn(t.Context(), task, false))

			for _, row := range producerRows(t, db) {
				require.Equal(t, "returned", row.State)
				require.Equal(t, "unproven", row.ContinuationState)
				require.GreaterOrEqual(t, row.ReturnedAtMs, row.StartedAtMs)
				require.NotEmpty(t, row.ReceiptJSON)
			}

			var uses []model.ChannelAdmissionUse
			require.NoError(t, db.Find(&uses).Error)

			for _, use := range uses {
				require.Equal(t, "open", use.State)
			}
		})
	}
}

func TestImageProducerPrivateSourceAndReleaseRetention(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installImageProducer(t, db)
	task, _ := producerFixture(t, f)
	copied, e := model.GetImageTask(task.ID, "g", 1)
	require.NoError(t, e)
	require.NoError(t, model.CaptureImageTaskGeneration(copied))
	require.Error(t, model.ReleaseImageTaskReservationGeneration(copied))
	_, e = model.PrepareImageTaskAttempt(t.Context(), copied, f.identities[0], 0)
	require.Error(t, e)
	require.Len(t, producerRows(t, db), 1)
	require.Len(t, runtimeUses(t, db), 1)
	require.NoError(t, model.ReleaseImageTaskReservationGeneration(task))
	// Until the original producer really returns, a same-ID reservation is not
	// allowed to displace the active source even after the task row was removed.
	original := &model.ImageTask{
		ID:                    task.ID,
		GroupID:               task.GroupID,
		TokenID:               task.TokenID,
		Model:                 task.Model,
		Fingerprint:           task.Fingerprint,
		BillingOperationID:    task.BillingOperationID,
		AdmissionIdentityHash: f.identities[0].IdentityHash,
		KeyFingerprint:        task.KeyFingerprint,
		ChannelType:           task.ChannelType,
	}
	_, _, e = model.ReserveImageTask(
		original,
		&model.AsyncUsageInfo{
			RequestID: task.ID,
			GroupID:   task.GroupID,
			TokenID:   task.TokenID,
			ChannelID: 1,
		},
	)
	require.Error(t, e)
	require.NoError(t, model.RecordImageProducerReturn(t.Context(), task, true))
	require.Equal(t, "closed", runtimeUses(t, db)[0].State)
}

func TestImageProducerStartFailureRollsBackReservationAndUse(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installImageProducer(t, db)
	require.NoError(t, db.AutoMigrate(&model.Log{}))

	seen := false
	require.NoError(
		t,
		db.Callback().
			Create().
			Before("gorm:create").
			Register("producer:insert-fail", func(tx *gorm.DB) {
				if tx.Statement.Table == "channel_admission_image_producers" {
					seen = true
					_ = tx.AddError(errors.New("synthetic producer start failure"))
				}
			}),
	)

	id := f.identities[0]
	_, created, e := model.ReserveImageTask(
		&model.ImageTask{
			ID:                    "failed-start",
			GroupID:               "g",
			TokenID:               1,
			Model:                 "image",
			Fingerprint:           strings.Repeat("a", 64),
			BillingOperationID:    "op",
			AdmissionIdentityHash: id.IdentityHash,
			KeyFingerprint:        id.KeyDigest,
		},
		&model.AsyncUsageInfo{GroupID: "g", TokenID: 1, ChannelID: 1},
	)
	require.Error(t, e)
	require.False(t, created)
	require.True(t, seen)

	for _, table := range []any{&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}, &model.ChannelAdmissionUse{}, &model.ChannelAdmissionImageProducer{}} {
		var count int64
		require.NoError(t, db.Model(table).Count(&count).Error)
		require.Zero(t, count)
	}
}
