package model_test

import (
	"path/filepath"
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const asyncScopeRequestID = "client-chosen-request-id"

func asyncScopeDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "async-scope.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	underlying, err := db.DB()
	require.NoError(t, err)

	previous := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = previous

		require.NoError(t, underlying.Close())
	})

	return db
}

// createAsyncScopeLog writes a call row under the shared request id. A zero
// status is a gateway rejection, which never carries an async usage status.
func createAsyncScopeLog(
	t *testing.T,
	db *gorm.DB,
	groupID string,
	tokenID int,
	callMode mode.Mode,
	status model.AsyncUsageStatus,
) model.Log {
	t.Helper()

	row := model.Log{
		RequestID:        asyncScopeRequestID,
		GroupID:          groupID,
		TokenID:          tokenID,
		Mode:             int(callMode),
		Code:             200,
		Content:          "untouched",
		AsyncUsageStatus: status,
	}
	if status == model.AsyncUsageStatusNone {
		row.Code = 429
	}

	require.NoError(t, db.Create(&row).Error)

	return row
}

func asyncScopeLogUpdates() map[string]func(*model.AsyncUsageInfo) error {
	return map[string]func(*model.AsyncUsageInfo) error{
		"usage": func(info *model.AsyncUsageInfo) error {
			return model.UpdateAsyncUsageLogUsage(
				info,
				model.Usage{OutputTokens: 7, TotalTokens: 7},
				model.UsageContext{},
				model.Price{},
				model.Amount{UsedAmount: 0.7},
				"USD",
				"v1",
			)
		},
		"failed": func(info *model.AsyncUsageInfo) error {
			return model.UpdateAsyncUsageLogFailed(info, "upstream task failed")
		},
		"status": func(info *model.AsyncUsageInfo) error {
			return model.UpdateAsyncUsageLogStatus(info, model.AsyncUsageStatusFailed)
		},
	}
}

func requireAsyncScopeLogUntouched(t *testing.T, db *gorm.DB, row model.Log) {
	t.Helper()

	var got model.Log
	require.NoError(t, db.First(&got, row.ID).Error)
	require.Equal(t, row.AsyncUsageStatus, got.AsyncUsageStatus, "row %d", row.ID)
	require.Equal(t, model.EmptyNullString("untouched"), got.Content, "row %d", row.ID)
	require.Zero(t, got.Amount.UsedAmount, "row %d", row.ID)
}

// Every async log update is scoped to the outbox's (request_id, group_id,
// token_id, mode). Rows written earlier under the same caller-chosen request id
// belong to other calls and must survive, whichever update runs.
func TestAsyncUsageLogUpdatesOnlyTouchTheOutboxCall(t *testing.T) {
	const (
		video   = mode.VideoGenerationsJobs
		pending = model.AsyncUsageStatusPending
	)

	for name, update := range asyncScopeLogUpdates() {
		t.Run(name, func(t *testing.T) {
			db := asyncScopeDB(t)

			others := []model.Log{
				// Another customer, even with a colliding token id.
				createAsyncScopeLog(t, db, "other-group", 1, video, pending),
				// Another key of the same customer.
				createAsyncScopeLog(t, db, "group", 2, video, pending),
				// Another kind of call by the same key.
				createAsyncScopeLog(t, db, "group", 1, mode.ImagesGenerations, pending),
				// A gateway rejection of the same key that reused the request id.
				createAsyncScopeLog(t, db, "group", 1, video, model.AsyncUsageStatusNone),
			}
			own := createAsyncScopeLog(t, db, "group", 1, video, pending)

			require.NoError(t, update(&model.AsyncUsageInfo{
				RequestID: asyncScopeRequestID,
				GroupID:   "group",
				TokenID:   1,
				Mode:      int(video),
			}))

			var got model.Log
			require.NoError(t, db.First(&got, own.ID).Error)
			require.NotEqual(t, model.AsyncUsageStatusPending, got.AsyncUsageStatus)

			for _, other := range others {
				requireAsyncScopeLogUntouched(t, db, other)
			}
		})
	}
}

// An outbox that lacks a token or mode keeps the keys it has and never reaches
// another customer's row.
func TestAsyncUsageLogUpdatesWithoutTokenOrModeStayInGroup(t *testing.T) {
	for name, update := range asyncScopeLogUpdates() {
		t.Run(name, func(t *testing.T) {
			db := asyncScopeDB(t)

			other := createAsyncScopeLog(
				t, db, "other-group", 1, mode.VideoGenerationsJobs, model.AsyncUsageStatusPending,
			)
			own := createAsyncScopeLog(
				t, db, "group", 1, mode.VideoGenerationsJobs, model.AsyncUsageStatusPending,
			)

			require.NoError(t, update(&model.AsyncUsageInfo{
				RequestID: asyncScopeRequestID,
				GroupID:   "group",
			}))

			var got model.Log
			require.NoError(t, db.First(&got, own.ID).Error)
			require.NotEqual(t, model.AsyncUsageStatusPending, got.AsyncUsageStatus)
			requireAsyncScopeLogUntouched(t, db, other)
		})
	}
}

// Accounting outboxes are bound to their own row: every attempt of a request
// shares the other keys, so a missing row id selects nothing rather than a
// sibling attempt.
func TestAsyncUsageLogUpdatesForAccountingOutboxNeedTheirRow(t *testing.T) {
	for name, update := range asyncScopeLogUpdates() {
		t.Run(name, func(t *testing.T) {
			db := asyncScopeDB(t)

			sibling := createAsyncScopeLog(
				t, db, "group", 1, mode.ImagesGenerations, model.AsyncUsageStatusPending,
			)
			own := createAsyncScopeLog(
				t, db, "group", 1, mode.ImagesGenerations, model.AsyncUsageStatusPending,
			)

			info := &model.AsyncUsageInfo{
				RequestID:     asyncScopeRequestID,
				GroupID:       "group",
				TokenID:       1,
				Mode:          int(mode.ImagesGenerations),
				MeasuredImage: true,
			}
			require.ErrorIs(t, update(info), gorm.ErrRecordNotFound)
			requireAsyncScopeLogUntouched(t, db, sibling)
			requireAsyncScopeLogUntouched(t, db, own)

			info.LogID = own.ID
			require.NoError(t, update(info))

			var got model.Log
			require.NoError(t, db.First(&got, own.ID).Error)
			require.NotEqual(t, model.AsyncUsageStatusPending, got.AsyncUsageStatus)
			requireAsyncScopeLogUntouched(t, db, sibling)
		})
	}
}
