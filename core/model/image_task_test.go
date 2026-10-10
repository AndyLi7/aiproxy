package model_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestImageReservationReplayAndOwnership(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "image.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	task := &model.ImageTask{
		ID:          "req-a",
		GroupID:     "group",
		TokenID:     1,
		Model:       "public",
		Fingerprint: "hash",
	}
	saved, created, err := model.ReserveImageTask(task, &model.AsyncUsageInfo{RequestID: task.ID})
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "submitting", saved.Status)

	_, created, err = model.ReserveImageTask(task, &model.AsyncUsageInfo{})
	require.NoError(t, err)
	require.False(t, created)

	conflict := *task
	conflict.Fingerprint = "different"
	_, _, err = model.ReserveImageTask(&conflict, &model.AsyncUsageInfo{})
	require.ErrorIs(t, err, model.ErrImageTaskConflict)
	_, err = model.GetImageTask("req-a", "group", 2)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)

	var logEntry model.Log
	require.NoError(t, db.First(&logEntry).Error)
	require.Equal(t, model.EmptyNullString(task.ID), logEntry.RequestID)

	var count int64
	require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	require.NoError(t, model.AcceptImageTask(task.ID, "provider-id"))

	var info model.AsyncUsageInfo
	require.NoError(t, db.First(&info).Error)
	require.Equal(t, model.AsyncUsageStatusPending, info.Status)
	require.Equal(t, "provider-id", info.UpstreamID)
}

func TestImageReservationPersistsTextOnlyLogSummary(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "image-summary.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}, &model.RequestDetail{}))
	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })
	_, created, err := model.ReserveImageTask(
		&model.ImageTask{ID: "summary", GroupID: "g", TokenID: 1, Model: "image", Fingerprint: "hash", RequestSummary: `{"prompt":"A quiet harbor"}`},
		&model.AsyncUsageInfo{RequestID: "summary"},
	)
	require.NoError(t, err)
	require.True(t, created)
	var entry model.Log
	require.NoError(t, db.Preload("RequestDetail").Where("request_id = ?", "summary").First(&entry).Error)
	require.NotNil(t, entry.RequestDetail)
	require.JSONEq(t, `{"prompt":"A quiet harbor"}`, entry.RequestDetail.RequestBody)
}

func TestImageReservationRollback(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "image.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	_, _, err = model.ReserveImageTask(&model.ImageTask{ID: "rollback"}, &model.AsyncUsageInfo{})
	require.Error(t, err)

	var count int64
	require.NoError(t, db.Model(&model.ImageTask{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestPendingImageTasksProtectChannelCredentials(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "channel.db"))
	require.NoError(t, err)
	require.NoError(
		t,
		db.AutoMigrate(
			&model.ImageTask{},
			&model.AsyncUsageInfo{},
			&model.Log{},
			&model.Channel{},
			&model.ChannelTest{},
		),
	)

	oldDB, oldLog := model.DB, model.LogDB
	model.DB, model.LogDB = db, db
	t.Cleanup(func() { model.DB, model.LogDB = oldDB, oldLog })

	ch := &model.Channel{Type: model.ChannelTypeFal, Key: "old"}
	require.NoError(t, db.Create(ch).Error)
	_, _, err = model.ReserveImageTask(
		&model.ImageTask{ID: "protected", GroupID: "g", TokenID: 1},
		&model.AsyncUsageInfo{ChannelID: ch.ID},
	)
	require.NoError(t, err)
	require.ErrorContains(t, model.DeleteChannelByID(ch.ID), "image tasks")
	require.ErrorContains(t, model.DeleteChannelsByIDs([]int{ch.ID}), "image tasks")

	replacement := "replacement"
	require.ErrorContains(
		t,
		model.UpdateChannel(ch, &model.ChannelPatch{Key: &replacement}),
		"image tasks",
	)
	current, err := model.GetChannelByID(ch.ID)
	require.NoError(t, err)
	require.Equal(t, "old", current.Key)
}

func TestImageReservationRejectsPreexistingOperationalRequestID(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "collision.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })
	// A request ID already logged for another API key is never adopted.
	require.NoError(t, db.Create(&model.Log{RequestID: "collision", GroupID: "other-group", TokenID: 9, Code: 404}).Error)
	_, _, err = model.ReserveImageTask(
		&model.ImageTask{ID: "collision", GroupID: "g", TokenID: 1},
		&model.AsyncUsageInfo{RequestID: "collision"},
	)
	require.ErrorIs(t, err, model.ErrImageTaskConflict)

	var count int64
	require.NoError(t, db.Model(&model.ImageTask{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestImageAccountingLogRetention(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "retention.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	info := &model.AsyncUsageInfo{RequestID: "retained", GroupID: "g"}
	_, _, err = model.ReserveImageTask(&model.ImageTask{ID: "retained", GroupID: "g"}, info)
	require.NoError(t, err)
	deleted, err := model.DeleteOldLog(time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Zero(t, deleted)
	deleted, err = model.DeleteGroupLogs("g")
	require.NoError(t, err)
	require.Zero(t, deleted)
	require.NoError(t, db.Model(info).Update("status", model.AsyncUsageStatusCompleted).Error)

	deleted, err = model.DeleteOldLog(time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
}

func TestInterruptedImageSubmissionStaysReserved(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "unknown.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	task := &model.ImageTask{ID: "lost", GroupID: "g", TokenID: 1}
	_, _, err = model.ReserveImageTask(task, &model.AsyncUsageInfo{})
	require.NoError(t, err)
	require.NoError(t, db.Model(task).Update("updated_at", time.Now().Add(-3*time.Minute)).Error)
	require.NoError(t, model.RecoverStaleImageSubmissions(time.Now()))

	saved, err := model.GetImageTask("lost", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "submission_unknown", saved.Status)

	_, created, err := model.ReserveImageTask(task, &model.AsyncUsageInfo{})
	require.NoError(t, err)
	require.False(t, created)
}

func TestCompleteSyncImageTaskActivatesAccountingAtomically(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "sync.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	task, created, err := model.ReserveImageTask(
		&model.ImageTask{ID: "sync", GroupID: "g", TokenID: 1, Model: "image", Fingerprint: "x"},
		&model.AsyncUsageInfo{RequestID: "sync"},
	)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(
		t,
		model.CompleteSyncImageTask(
			task.ID,
			[]model.ImageOutput{{URL: "https://example.com/a.png"}},
		),
	)
	got, err := model.GetImageTask(task.ID, "g", 1)
	require.NoError(t, err)
	require.Equal(t, "completed", got.Status)

	var info model.AsyncUsageInfo
	require.NoError(t, db.First(&info).Error)
	require.Equal(t, model.AsyncUsageStatusPending, info.Status)
	require.Error(
		t,
		model.CompleteSyncImageTask(
			task.ID,
			[]model.ImageOutput{{URL: "https://example.com/b.png"}},
		),
	)
	got, err = model.GetImageTask(task.ID, "g", 1)
	require.NoError(t, err)
	require.Equal(t, "https://example.com/a.png", got.Data[0].URL)
}

func TestImageReservationAfterEndpointRejection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*model.Log)
		allowed bool
	}{
		{"safe fallback", func(l *model.Log) {}, true},
		{"other owner", func(l *model.Log) { l.TokenID = 2 }, false},
		{"other group", func(l *model.Log) { l.GroupID = "other" }, false},
		// Any gateway-only rejection of the same caller is harmless: it never
		// reached a provider, so the caller may fix the request and retry the ID.
		{"other model", func(l *model.Log) { l.Model = "other" }, true},
		{"other validation", func(l *model.Log) { l.ErrorCode = "invalid_parameter" }, true},
		{"gateway status without provider", func(l *model.Log) { l.Code = 504 }, true},
		{"insufficient balance", func(l *model.Log) {
			l.Code, l.ErrorCode, l.FailureStage = 402, "insufficient_balance", model.FailureStageBalance
		}, true},
		{"rate limited", func(l *model.Log) {
			l.Code, l.ErrorCode, l.FailureStage = 429, "rate_limited", model.FailureStageRateLimit
		}, true},
		{"model not found", func(l *model.Log) {
			l.Code, l.ErrorCode, l.FailureStage = 404, "model_unavailable", model.FailureStageModel
		}, true},
		{"unauthenticated", func(l *model.Log) {
			l.GroupID, l.TokenID, l.Code, l.ErrorCode, l.FailureStage = "", 0, 401, "invalid_api_key", model.FailureStageAuth
		}, true},
		{"successful request", func(l *model.Log) { l.Code, l.ErrorCode, l.FailureStage = 200, "", "" }, false},
		{"upstream attempted", func(l *model.Log) { l.ChannelID = 1 }, false},
		{"upstream accepted", func(l *model.Log) { l.UpstreamID = "accepted" }, false},
		{"charged", func(l *model.Log) { l.Amount.UsedAmount = 0.01 }, false},
		{"uncertain stage", func(l *model.Log) { l.FailureStage = model.FailureStageUpstream }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "fallback.db"))
			require.NoError(t, err)
			require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))
			old := model.LogDB
			model.LogDB = db
			t.Cleanup(func() { model.LogDB = old })
			prior := &model.Log{RequestID: "fallback", GroupID: "g", TokenID: 1, Model: "wan", Capability: "text-to-image", Code: 400, ErrorCode: "unsupported_endpoint", FailureStage: model.FailureStageValidation, Endpoint: "POST /v1/images/generations"}
			tc.mutate(prior)
			require.NoError(t, db.Create(prior).Error)
			task := &model.ImageTask{ID: "fallback", GroupID: "g", TokenID: 1, Model: "wan/text-to-image", Fingerprint: "same"}
			_, created, err := model.ReserveImageTask(task, &model.AsyncUsageInfo{RequestID: task.ID})
			if !tc.allowed {
				require.ErrorIs(t, err, model.ErrImageTaskConflict)
				var n int64
				require.NoError(t, db.Model(&model.ImageTask{}).Count(&n).Error)
				require.Zero(t, n)
				return
			}
			require.NoError(t, err)
			require.True(t, created)
			_, created, err = model.ReserveImageTask(task, &model.AsyncUsageInfo{RequestID: task.ID})
			require.NoError(t, err)
			require.False(t, created)
			changed := *task
			changed.Fingerprint = "changed"
			_, _, err = model.ReserveImageTask(&changed, &model.AsyncUsageInfo{})
			require.ErrorIs(t, err, model.ErrImageTaskConflict)
			var n int64
			require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Count(&n).Error)
			require.EqualValues(t, 1, n)
			require.NoError(t, db.Model(&model.Log{}).Count(&n).Error)
			require.EqualValues(t, 2, n)
			require.NoError(t, model.AcceptImageTask(task.ID, "upstream"))
			require.NoError(t, model.SetImageTaskResult(task.ID, "failed", nil, &model.ImageTaskError{Code: "test", Message: "test"}))
			var preserved model.Log
			require.NoError(t, db.First(&preserved, prior.ID).Error)
			require.Equal(t, prior.Code, preserved.Code)
			require.Empty(t, preserved.UpstreamID)
			require.Zero(t, preserved.Amount.UsedAmount)

		})
	}
}

func TestImagePhaseTimesDoNotRegressOrInventHistory(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "phases.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))
	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })
	task := &model.ImageTask{ID: "phases", GroupID: "g", TokenID: 1, ArchiveRequired: true}
	_, _, err = model.ReserveImageTask(task, &model.AsyncUsageInfo{RequestID: task.ID})
	require.NoError(t, err)
	require.NoError(t, model.AcceptImageTask(task.ID, "provider"))
	require.NoError(t, model.SetImageTaskResult(task.ID, "in_progress", nil, nil))
	var first model.ImageTask
	require.NoError(t, db.First(&first, "id = ?", task.ID).Error)
	require.NotNil(t, first.QueuedAt)
	require.NotNil(t, first.RunningAt)
	require.NoError(t, model.SetImageTaskResult(task.ID, "queued", nil, nil))
	require.NoError(t, model.SetImageTaskResult(task.ID, "in_progress", nil, nil))
	var next model.ImageTask
	require.NoError(t, db.First(&next, "id = ?", task.ID).Error)
	require.Equal(t, "in_progress", next.Status)
	require.True(t, first.RunningAt.Equal(*next.RunningAt))
	require.NoError(t, model.SetImageTaskResult(task.ID, "result_processing", []model.ImageOutput{{URL: "https://source/image"}}, nil))
	require.NoError(t, model.SetImageTaskResult(task.ID, "in_progress", nil, nil))
	require.NoError(t, db.First(&next, "id = ?", task.ID).Error)
	require.Equal(t, "result_processing", next.Status)
	require.Len(t, next.Data, 1)
	require.NotNil(t, next.ResultReceivedAt)
	require.Nil(t, next.CompletedAt)
	legacy := &model.ImageTask{ID: "legacy", Status: "completed"}
	require.NoError(t, db.Create(legacy).Error)
	require.Nil(t, legacy.RunningAt)
	require.Nil(t, legacy.ResultExpiresAt)
}

func TestResultBillingMetadataPersistsAndTerminalReplayCannotReplaceIt(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "result-metadata.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))
	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })
	require.NoError(t, db.Create(&model.ImageTask{ID: "metadata", GroupID: "g", TokenID: 1, Status: "queued"}).Error)
	seed := int64(42)
	require.NoError(t, model.SetImageTaskResult("metadata", "completed", []model.ImageOutput{{URL: "https://example.com/image.png", RevisedPrompt: "revised"}}, nil, model.ImageResultMetadata{NumImages: &seed, Description: "generated explanation", Seed: &seed, BillableUnits: "1.5"}))
	require.NoError(t, model.SetImageTaskResult("metadata", "completed", []model.ImageOutput{{URL: "https://example.com/other.png"}}, nil, model.ImageResultMetadata{BillableUnits: "999"}))
	saved, err := model.GetImageTask("metadata", "g", 1)
	require.NoError(t, err)
	require.Equal(t, int64(42), *saved.Seed)
	require.Equal(t, "1.5", saved.BillableUnits)
	require.Equal(t, "revised", saved.Data[0].RevisedPrompt)
	require.Equal(t, "generated explanation", saved.Description)
	require.Equal(t, int64(42), *saved.NumImages)
	encoded, err := json.Marshal(saved)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"num_images":42`)
}

func TestLargeSeedSurvivesPersistenceAndPublicJSONExactly(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "large-seed.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))
	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })
	require.NoError(t, db.Create(&model.ImageTask{ID: "large", GroupID: "g", TokenID: 1, Status: "queued"}).Error)
	exact := "18446744073709551615"
	seed := json.Number(exact)
	require.NoError(t, model.SetImageTaskResult("large", "completed", []model.ImageOutput{{URL: "https://example.com/a.png", Seed: &seed}}, nil, model.ImageResultMetadata{SeedExact: exact}))
	saved, err := model.GetImageTask("large", "g", 1)
	require.NoError(t, err)
	require.Equal(t, exact, saved.SeedExact)
	encoded, err := json.Marshal(saved)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"seed":`+exact)
	require.NotContains(t, string(encoded), "seed_exact")
	require.Equal(t, seed, *saved.Data[0].Seed)
}

func TestReleasedImageReservationCanBeReservedAgain(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "release.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}, &model.RequestDetail{}))
	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	reserve := func(operation string) (*model.ImageTask, bool) {
		task, created, err := model.ReserveImageTask(
			&model.ImageTask{ID: "topup", GroupID: "g", TokenID: 1, Model: "image", Fingerprint: "same", BillingOperationID: operation, RequestSummary: `{"prompt":"p"}`},
			&model.AsyncUsageInfo{RequestID: "topup", ChannelID: 7, BillingOperationID: operation},
		)
		require.NoError(t, err)
		return task, created
	}

	_, created := reserve("first-operation")
	require.True(t, created)
	require.NoError(t, model.ReleaseImageTaskReservation("topup"))

	for _, table := range []any{&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}, &model.RequestDetail{}} {
		var n int64
		require.NoError(t, db.Model(table).Count(&n).Error)
		require.Zero(t, n, "%T", table)
	}
	_, err = model.GetImageTask("topup", "g", 1)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)

	// After a top-up the same ID and body reserve a fresh billing operation.
	task, created := reserve("second-operation")
	require.True(t, created)
	require.Equal(t, "second-operation", task.BillingOperationID)
}

func TestImageReservationIsNotReleasedOnceItMayHaveReachedAProvider(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *gorm.DB)
	}{
		{"accepted", func(t *testing.T, _ *gorm.DB) { require.NoError(t, model.AcceptImageTask("kept", "provider-id")) }},
		{"submission unknown", func(t *testing.T, db *gorm.DB) {
			require.NoError(t, db.Model(&model.ImageTask{}).Where("id = ?", "kept").Update("status", "submission_unknown").Error)
		}},
		{"attempt started", func(t *testing.T, db *gorm.DB) {
			require.NoError(t, db.Model(&model.ImageTask{}).Where("id = ?", "kept").Update("attempts", `[{"channel_id":7}]`).Error)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "kept.db"))
			require.NoError(t, err)
			require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))
			old := model.LogDB
			model.LogDB = db
			t.Cleanup(func() { model.LogDB = old })

			_, created, err := model.ReserveImageTask(
				&model.ImageTask{ID: "kept", GroupID: "g", TokenID: 1, Model: "image", Fingerprint: "same"},
				&model.AsyncUsageInfo{RequestID: "kept", ChannelID: 7},
			)
			require.NoError(t, err)
			require.True(t, created)
			tc.mutate(t, db)

			require.Error(t, model.ReleaseImageTaskReservation("kept"))
			for _, table := range []any{&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}} {
				var n int64
				require.NoError(t, db.Model(table).Count(&n).Error)
				require.EqualValues(t, 1, n, "%T", table)
			}
		})
	}
}
