//nolint:testpackage // These fixtures verify internal metering and persistence boundaries.
package task

import (
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestMeasuredSettlementReplayUsesImmutableSnapshot(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}, &model.ConsumeError{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	consumer := &replaySafeAmbiguousAsyncUsageConsumer{charges: map[string]int{}}
	oldB := balance.Default
	balance.Default = replaySafeAmbiguousAsyncUsageBalance{consumer: consumer}
	t.Cleanup(func() { balance.Default = oldB })
	require.NoError(
		t,
		model.CacheSetGroup(&model.GroupCache{ID: "measured", Status: model.GroupStatusEnabled}),
	)
	t.Cleanup(func() { _ = model.CacheDeleteGroup("measured") })

	w := int64(10)
	micros := int64(300000)
	info := &model.AsyncUsageInfo{
		RequestID: "measured",
		GroupID:   "measured",
		RequestAt: time.Now(),
		TokenID:   1,
		Price: model.Price{
			OutputPrice:     9,
			OutputPriceUnit: 1,
			ImageBilling:    &model.ImageBillingPolicy{Version: 1, Scenario: "generation"},
		},
		UsageContext: model.UsageContext{
			ImageUsage: &model.ImageUsage{
				Version:  1,
				State:    "complete",
				Scenario: "generation",
				Outputs:  []model.ImageUsageOutput{{Index: 0, Width: &w, Height: &w}},
			},
		},
		Amount: model.Amount{
			UsedAmount: .3,
			ImageBillingResult: &model.ImageBillingResult{
				State:        "complete",
				AmountMicros: &micros,
				Lines:        []model.ImageBillingLine{},
			},
		},
	}
	require.NoError(t, model.CreateMeasuredImageSettlement(&model.Log{RequestID: "measured"}, info))
	info.NextPollAt = time.Now().Add(-time.Second)
	require.NoError(t, model.UpdateAsyncUsageInfo(info))
	ok, err := model.TryClaimAsyncUsageInfo(info, "claim", time.Now().Add(time.Minute), time.Now())
	require.NoError(t, err)
	require.True(t, ok)
	require.ErrorIs(
		t,
		completeAsyncUsage(t.Context(), info, model.Usage{}, model.UsageContext{}),
		errAsyncUsageSettlementPending,
	)

	var stored model.AsyncUsageInfo
	require.NoError(t, db.First(&stored, info.ID).Error)
	require.NoError(
		t,
		completeAsyncUsage(
			t.Context(),
			&stored,
			model.Usage{OutputTokens: 999},
			model.UsageContext{},
		),
	)
	require.Equal(t, []float64{.3, .3}, consumer.amounts)
	require.Equal(t, 1, consumer.charges["measured"])

	var entry model.Log
	require.NoError(t, db.First(&entry, info.LogID).Error)
	require.Equal(t, model.AsyncUsageStatusCompleted, entry.AsyncUsageStatus)
	require.Equal(t, micros, *entry.Amount.ImageBillingResult.AmountMicros)
	require.Len(t, entry.UsageContext.ImageUsage.Outputs, 1)
}

func TestMeasuredWorkerNeverPollsForZeroOrIncomplete(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	for _, state := range []string{"complete", "pending"} {
		z := int64(0)
		info := &model.AsyncUsageInfo{
			RequestID:       state,
			MeasuredImage:   true,
			ProcessingToken: "claim",
			Amount: model.Amount{
				ImageBillingResult: &model.ImageBillingResult{State: state, AmountMicros: &z},
			},
		}
		require.NoError(t, model.CreateAsyncUsageInfo(info))

		entry := &model.Log{RequestID: model.EmptyNullString(state)}
		require.NoError(t, db.Create(entry).Error)
		info.LogID = entry.ID
		require.NoError(t, model.UpdateAsyncUsageInfo(info))
		processOneAsyncUsage(t.Context(), info)

		var got model.AsyncUsageInfo
		require.NoError(t, db.First(&got, info.ID).Error)

		if state == "complete" {
			require.Equal(t, model.AsyncUsageStatusCompleted, got.Status)
		} else {
			require.Equal(t, model.AsyncUsageStatusMeasurementPending, got.Status)
		}
	}
}

func TestMeasuredFailureOnlyUpdatesPairedLog(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	earlier := model.Log{
		RequestID:        "shared",
		Content:          "earlier immutable failure",
		AsyncUsageStatus: model.AsyncUsageStatusFailed,
	}
	paired := model.Log{
		RequestID:        "shared",
		Content:          "final",
		AsyncUsageStatus: model.AsyncUsageStatusPending,
	}

	require.NoError(t, db.Create(&earlier).Error)
	require.NoError(t, db.Create(&paired).Error)
	info := &model.AsyncUsageInfo{
		RequestID:       "shared",
		MeasuredImage:   true,
		LogID:           paired.ID,
		ProcessingToken: "claim",
	}
	require.NoError(t, model.CreateAsyncUsageInfo(info))
	markAsyncUsageFailed(info, "terminal settlement error")

	var got model.Log
	require.NoError(t, db.First(&got, earlier.ID).Error)
	require.Equal(t, earlier.Content, got.Content)
	got = model.Log{}
	require.NoError(t, db.First(&got, paired.ID).Error)
	require.Equal(t, model.EmptyNullString("terminal settlement error"), got.Content)
	require.Equal(t, model.AsyncUsageStatusFailed, got.AsyncUsageStatus)
}
