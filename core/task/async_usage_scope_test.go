//nolint:testpackage // Settlement entry points are internal to the worker.
package task

import (
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// The request id is chosen by the caller (X-Request-ID), so another customer,
// or another key of the same customer, can log a call with the same id.
// Settlement must only ever touch the row of the call that created the outbox.
const sharedAsyncRequestID = "client-chosen-request-id"

func asyncScopeLogDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	return db
}

// foreignAsyncLogs writes, before the owner's row, the rows an unscoped
// request_id lookup would hit first.
func foreignAsyncLogs(t *testing.T, db *gorm.DB, groupID string) []model.Log {
	t.Helper()

	rows := []model.Log{
		// Another customer.
		{GroupID: "another-group", TokenID: 1, Mode: int(mode.VideoGenerationsJobs)},
		// Another key of the same customer.
		{GroupID: groupID, TokenID: 2, Mode: int(mode.VideoGenerationsJobs)},
	}
	for i := range rows {
		rows[i].RequestID = sharedAsyncRequestID
		rows[i].Code = 200
		rows[i].Content = "untouched"
		rows[i].AsyncUsageStatus = model.AsyncUsageStatusPending
		require.NoError(t, db.Create(&rows[i]).Error)
	}

	return rows
}

func requireForeignAsyncLogsUntouched(t *testing.T, db *gorm.DB, rows []model.Log) {
	t.Helper()

	for _, row := range rows {
		var got model.Log
		require.NoError(t, db.First(&got, row.ID).Error)
		require.Equal(
			t,
			model.AsyncUsageStatusPending,
			got.AsyncUsageStatus,
			"group %s token %d",
			row.GroupID,
			row.TokenID,
		)
		require.Equal(t, model.EmptyNullString("untouched"), got.Content)
		require.Zero(t, got.Usage.OutputTokens)
		require.Zero(t, got.Amount.UsedAmount)
	}
}

func TestCompleteAsyncUsageSettlesOnlyTheOutboxOwnersLog(t *testing.T) {
	db := asyncScopeLogDB(t)

	const groupID = "group-async-scope"
	require.NoError(t, model.CacheSetGroup(&model.GroupCache{
		ID:     groupID,
		Status: model.GroupStatusEnabled,
	}))
	t.Cleanup(func() { require.NoError(t, model.CacheDeleteGroup(groupID)) })

	consumer := &pricingCaptureConsumer{}
	oldBalance := balance.Default
	balance.Default = pricingCaptureBalance{consumer: consumer}
	t.Cleanup(func() { balance.Default = oldBalance })

	foreign := foreignAsyncLogs(t, db, groupID)
	own := model.Log{
		RequestID:        sharedAsyncRequestID,
		GroupID:          groupID,
		TokenID:          1,
		Mode:             int(mode.VideoGenerationsJobs),
		Code:             200,
		AsyncUsageStatus: model.AsyncUsageStatusPending,
	}
	require.NoError(t, db.Create(&own).Error)

	info := &model.AsyncUsageInfo{
		RequestID:       sharedAsyncRequestID,
		RequestAt:       time.Now(),
		Mode:            int(mode.VideoGenerationsJobs),
		Model:           "video-model",
		GroupID:         groupID,
		TokenID:         1,
		Price:           model.Price{OutputPrice: 0.5, OutputPriceUnit: 1},
		ProcessingToken: "claim-token",
	}
	require.NoError(t, model.CreateAsyncUsageInfo(info))

	require.NoError(t, completeAsyncUsage(
		t.Context(),
		info,
		model.Usage{OutputTokens: 4, TotalTokens: 4},
		model.UsageContext{},
	))
	require.Equal(t, 1, consumer.attempts)
	requireForeignAsyncLogsUntouched(t, db, foreign)

	var got model.Log
	require.NoError(t, db.First(&got, own.ID).Error)
	require.Equal(t, model.AsyncUsageStatusCompleted, got.AsyncUsageStatus)
	require.Equal(t, model.ZeroNullInt64(4), got.Usage.OutputTokens)
	require.InDelta(t, 2.0, got.Amount.UsedAmount, 1e-9)
}

func TestMarkAsyncUsageFailedFailsOnlyTheOutboxOwnersLog(t *testing.T) {
	const groupID = "group-async-scope"

	cases := map[string]struct {
		info func(own model.Log) *model.AsyncUsageInfo
		mode mode.Mode
		code int
	}{
		"video": {
			mode: mode.VideoGenerationsJobs,
			code: 200,
			info: func(model.Log) *model.AsyncUsageInfo {
				return &model.AsyncUsageInfo{Mode: int(mode.VideoGenerationsJobs)}
			},
		},
		// The image task outbox knows its exact row; a rejection of the same key
		// that reused the request id is not that row.
		"image task": {
			mode: mode.ImagesGenerations,
			code: 202,
			info: func(own model.Log) *model.AsyncUsageInfo {
				return &model.AsyncUsageInfo{
					Mode:        int(mode.ImagesGenerations),
					ImageTaskID: sharedAsyncRequestID,
					LogID:       own.ID,
				}
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := asyncScopeLogDB(t)
			foreign := foreignAsyncLogs(t, db, groupID)

			rejected := model.Log{
				RequestID: sharedAsyncRequestID,
				GroupID:   groupID,
				TokenID:   1,
				Mode:      int(tc.mode),
				Code:      429,
				Content:   "rate limited",
			}
			require.NoError(t, db.Create(&rejected).Error)

			own := model.Log{
				RequestID:        sharedAsyncRequestID,
				GroupID:          groupID,
				TokenID:          1,
				Mode:             int(tc.mode),
				Code:             tc.code,
				AsyncUsageStatus: model.AsyncUsageStatusPending,
			}
			require.NoError(t, db.Create(&own).Error)

			info := tc.info(own)
			info.RequestID = sharedAsyncRequestID
			info.GroupID = groupID
			info.TokenID = 1
			info.Status = model.AsyncUsageStatusPending
			info.ProcessingToken = "claim-token"
			info.NextPollAt = time.Now().Add(time.Minute)
			require.NoError(t, db.Create(info).Error)
			if info.ImageTaskID != "" {
				require.NoError(t, db.AutoMigrate(&model.ImageTask{}))
				require.NoError(t, db.Create(&model.ImageTask{ID: info.ImageTaskID, GroupID: info.GroupID, TokenID: info.TokenID, UsageID: info.ID, Status: "failed"}).Error)
			}

			markAsyncUsageFailed(info, "upstream task failed")
			requireForeignAsyncLogsUntouched(t, db, foreign)

			var got model.Log
			require.NoError(t, db.First(&got, own.ID).Error)
			require.Equal(t, model.AsyncUsageStatusFailed, got.AsyncUsageStatus)
			require.Equal(t, model.EmptyNullString("upstream task failed"), got.Content)

			got = model.Log{}
			require.NoError(t, db.First(&got, rejected.ID).Error)
			require.Equal(t, model.AsyncUsageStatusNone, got.AsyncUsageStatus)
			require.Equal(t, model.EmptyNullString("rate limited"), got.Content)
		})
	}
}
