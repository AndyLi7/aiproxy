//nolint:testpackage
package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type failingAsyncUsageBalance struct {
	err error
}

func (b failingAsyncUsageBalance) GetGroupRemainBalance(
	context.Context,
	model.GroupCache,
) (float64, balance.PostGroupConsumer, error) {
	return 100, failingAsyncUsageConsumer(b), nil
}

func (b failingAsyncUsageBalance) GetGroupQuota(
	context.Context,
	model.GroupCache,
) (*balance.GroupQuota, error) {
	return &balance.GroupQuota{Total: 100, Remain: 100}, nil
}

type failingAsyncUsageConsumer struct {
	err error
}

func (c failingAsyncUsageConsumer) PostGroupConsume(
	context.Context,
	string,
	float64,
) (float64, error) {
	return 0, c.err
}

type replaySafeAmbiguousAsyncUsageConsumer struct {
	attempts int
	charges  map[string]int
	amounts  []float64
}

func (c *replaySafeAmbiguousAsyncUsageConsumer) PostGroupConsume(
	ctx context.Context,
	_ string,
	amount float64,
) (float64, error) {
	c.attempts++
	c.amounts = append(c.amounts, amount)
	requestID := balance.RequestIDFromContext(ctx)
	if c.charges[requestID] == 0 {
		c.charges[requestID] = 1

		return 0, errors.New("wallet response lost after durable debit")
	}

	return amount, nil
}

func (*replaySafeAmbiguousAsyncUsageConsumer) CanReplayPostGroupConsume(
	ctx context.Context,
) bool {
	return balance.RequestIDFromContext(ctx) != ""
}

type replaySafeAmbiguousAsyncUsageBalance struct {
	consumer *replaySafeAmbiguousAsyncUsageConsumer
}

func (b replaySafeAmbiguousAsyncUsageBalance) GetGroupRemainBalance(
	context.Context,
	model.GroupCache,
) (float64, balance.PostGroupConsumer, error) {
	return 100, b.consumer, nil
}

func (replaySafeAmbiguousAsyncUsageBalance) GetGroupQuota(
	context.Context,
	model.GroupCache,
) (*balance.GroupQuota, error) {
	return &balance.GroupQuota{Total: 100, Remain: 100}, nil
}

type preChargeFailingAsyncUsageBalance struct {
	err error
}

type pricingCaptureConsumer struct {
	currency string
	version  string
	attempts int
}

type finalizedLogConsumer struct {
	t         *testing.T
	db        *gorm.DB
	requestID string
	sawUsage  bool
}

func (c *finalizedLogConsumer) PostGroupConsume(
	_ context.Context,
	_ string,
	amount float64,
) (float64, error) {
	var got model.Log
	require.NoError(c.t, c.db.Where("request_id = ?", c.requestID).First(&got).Error)
	c.sawUsage = int64(got.Usage.TotalTokens) > 0 && got.Amount.UsedAmount == amount

	return amount, nil
}

type finalizedLogBalance struct {
	consumer *finalizedLogConsumer
}

func (b finalizedLogBalance) GetGroupRemainBalance(
	context.Context,
	model.GroupCache,
) (float64, balance.PostGroupConsumer, error) {
	return 100, b.consumer, nil
}

func (finalizedLogBalance) GetGroupQuota(
	context.Context,
	model.GroupCache,
) (*balance.GroupQuota, error) {
	return &balance.GroupQuota{Total: 100, Remain: 100}, nil
}

func (c *pricingCaptureConsumer) PostGroupConsume(
	ctx context.Context,
	_ string,
	amount float64,
) (float64, error) {
	c.attempts++
	c.currency, c.version = balance.PricingFromContext(ctx)
	return amount, nil
}

type pricingCaptureBalance struct {
	consumer *pricingCaptureConsumer
}

func (b pricingCaptureBalance) GetGroupRemainBalance(
	context.Context,
	model.GroupCache,
) (float64, balance.PostGroupConsumer, error) {
	return 100, b.consumer, nil
}

func (pricingCaptureBalance) GetGroupQuota(
	context.Context,
	model.GroupCache,
) (*balance.GroupQuota, error) {
	return &balance.GroupQuota{Total: 100, Remain: 100}, nil
}

func (b preChargeFailingAsyncUsageBalance) GetGroupRemainBalance(
	context.Context,
	model.GroupCache,
) (float64, balance.PostGroupConsumer, error) {
	return 0, nil, b.err
}

func (b preChargeFailingAsyncUsageBalance) GetGroupQuota(
	context.Context,
	model.GroupCache,
) (*balance.GroupQuota, error) {
	return nil, b.err
}

func TestConsumeAsyncUsagePassesPricingProvenance(t *testing.T) {
	consumer := &pricingCaptureConsumer{}
	oldBalance := balance.Default
	balance.Default = pricingCaptureBalance{consumer: consumer}
	t.Cleanup(func() { balance.Default = oldBalance })

	const groupID = "group-pricing-provenance"
	require.NoError(t, model.CacheSetGroup(&model.GroupCache{
		ID:     groupID,
		Status: model.GroupStatusEnabled,
	}))
	t.Cleanup(func() { require.NoError(t, model.CacheDeleteGroup(groupID)) })

	charged, replaySafe, err := consumeAsyncUsageGroupBalance(t.Context(), &model.AsyncUsageInfo{
		RequestID:       "pricing_request",
		GroupID:         groupID,
		TokenName:       "token-1",
		PricingCurrency: "USD",
		PricingVersion:  "21",
	}, 0.108607)
	require.NoError(t, err)
	require.True(t, charged)
	require.False(t, replaySafe)
	require.Equal(t, "USD", consumer.currency)
	require.Equal(t, "21", consumer.version)
}

func TestCompleteAsyncUsageIgnoresMissingLog(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = oldLogDB
	})

	info := &model.AsyncUsageInfo{
		RequestID:       "missing_log",
		RequestAt:       time.Now(),
		Status:          model.AsyncUsageStatusPending,
		Model:           "gpt-5.4",
		ProcessingToken: "claim-token",
	}
	require.NoError(t, model.CreateAsyncUsageInfo(info))

	usage := model.Usage{
		InputTokens: 10,
		TotalTokens: 10,
	}

	require.NoError(t, completeAsyncUsage(context.Background(), info, usage, model.UsageContext{}))
	require.Equal(t, model.AsyncUsageStatusCompleted, info.Status)
	require.Equal(t, usage.InputTokens, info.Usage.InputTokens)
}

func TestCompleteAsyncUsagePersistsFinalUsageBeforeBalanceCallback(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = oldLogDB })

	const (
		requestID = "usage_before_balance_callback"
		groupID   = "group-usage-before-balance-callback"
	)
	require.NoError(t, db.Create(&model.Log{
		RequestID:        model.EmptyNullString(requestID),
		AsyncUsageStatus: model.AsyncUsageStatusPending,
	}).Error)
	require.NoError(t, model.CacheSetGroup(&model.GroupCache{
		ID:     groupID,
		Status: model.GroupStatusEnabled,
	}))
	t.Cleanup(func() { require.NoError(t, model.CacheDeleteGroup(groupID)) })

	consumer := &finalizedLogConsumer{t: t, db: db, requestID: requestID}
	oldBalance := balance.Default
	balance.Default = finalizedLogBalance{consumer: consumer}
	t.Cleanup(func() { balance.Default = oldBalance })

	info := &model.AsyncUsageInfo{
		RequestID:       requestID,
		RequestAt:       time.Now(),
		Status:          model.AsyncUsageStatusPending,
		Model:           "video-model",
		GroupID:         groupID,
		TokenID:         1,
		TokenName:       "playground",
		Price:           model.Price{OutputPrice: 0.5, OutputPriceUnit: 1},
		ProcessingToken: "claim-token",
	}
	require.NoError(t, model.CreateAsyncUsageInfo(info))

	require.NoError(t, completeAsyncUsage(
		context.Background(),
		info,
		model.Usage{OutputTokens: 4, TotalTokens: 4},
		model.UsageContext{},
	))
	require.True(t, consumer.sawUsage, "wallet callback must observe finalized usage and amount")
}

func TestCompleteAsyncUsageReturnsLogUpdateError(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = oldLogDB
	})

	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	info := &model.AsyncUsageInfo{
		RequestID: "log_update_error",
		RequestAt: time.Now(),
		Status:    model.AsyncUsageStatusPending,
		Model:     "gpt-5.4",
	}
	usage := model.Usage{
		InputTokens: 10,
		TotalTokens: 10,
	}

	require.Error(t, completeAsyncUsage(context.Background(), info, usage, model.UsageContext{}))
	require.Equal(t, model.AsyncUsageStatusPending, info.Status)
	require.Equal(t, model.ZeroNullInt64(0), info.Usage.InputTokens)
}

func TestCompleteAsyncUsageRecordsBalanceConsumeErrorWithoutRetry(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.Log{},
		&model.AsyncUsageInfo{},
		&model.ConsumeError{},
	))

	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = oldLogDB
	})

	oldBalance := balance.Default
	balance.Default = failingAsyncUsageBalance{err: errors.New("balance unavailable")}
	t.Cleanup(func() {
		balance.Default = oldBalance
	})

	require.NoError(t, model.CacheSetGroup(&model.GroupCache{
		ID:     "group-async-balance",
		Status: model.GroupStatusEnabled,
	}))
	t.Cleanup(func() {
		require.NoError(t, model.CacheDeleteGroup("group-async-balance"))
	})

	info := &model.AsyncUsageInfo{
		RequestID:       "balance_error",
		RequestAt:       time.Now(),
		Status:          model.AsyncUsageStatusPending,
		Model:           "gpt-5.4",
		GroupID:         "group-async-balance",
		TokenID:         1,
		TokenName:       "token-1",
		Price:           model.Price{InputPrice: 1, InputPriceUnit: 1},
		UpstreamID:      "resp_balance_error",
		ProcessingToken: "balance-error-claim",
	}
	require.NoError(t, model.CreateAsyncUsageInfo(info))

	err = completeAsyncUsage(context.Background(), info, model.Usage{
		InputTokens: 10,
		TotalTokens: 10,
	}, model.UsageContext{})
	require.NoError(t, err)
	require.Equal(t, model.AsyncUsageStatusCompleted, info.Status)
	require.False(t, info.BalanceConsumed)

	var consumeErrors []model.ConsumeError
	require.NoError(t, db.Find(&consumeErrors).Error)
	require.Len(t, consumeErrors, 1)
	require.Equal(t, "balance_error", consumeErrors[0].RequestID)
	require.Equal(t, float64(10), consumeErrors[0].UsedAmount)

	var got model.AsyncUsageInfo
	require.NoError(t, db.First(&got, info.ID).Error)
	require.Equal(t, model.AsyncUsageStatusCompleted, got.Status)
	require.False(t, got.BalanceConsumed)
	require.True(t, got.BalanceConsumeAttempted)
	require.Equal(t, float64(10), got.Amount.UsedAmount)
}

func TestCompleteAsyncUsageDoesNotReplaySuccessfulNonIdempotentDebitWhenMarkerWriteFails(
	t *testing.T,
) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.Log{},
		&model.AsyncUsageInfo{},
		&model.ConsumeError{},
	))

	sqlDB, err := db.DB()
	require.NoError(t, err)
	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = oldLogDB
		require.NoError(t, sqlDB.Close())
	})

	consumer := &pricingCaptureConsumer{}
	oldBalance := balance.Default
	balance.Default = pricingCaptureBalance{consumer: consumer}
	t.Cleanup(func() { balance.Default = oldBalance })

	oldMark := markAsyncUsageBalanceConsumed
	markAsyncUsageBalanceConsumed = func(*model.AsyncUsageInfo) error {
		return errors.New("marker write unavailable")
	}
	t.Cleanup(func() { markAsyncUsageBalanceConsumed = oldMark })

	const (
		requestID = "non_idempotent_marker_failure"
		groupID   = "group-non-idempotent-marker-failure"
	)
	require.NoError(t, model.CacheSetGroup(&model.GroupCache{
		ID:     groupID,
		Status: model.GroupStatusEnabled,
	}))
	t.Cleanup(func() { require.NoError(t, model.CacheDeleteGroup(groupID)) })
	require.NoError(t, db.Create(&model.Log{
		RequestID:        model.EmptyNullString(requestID),
		AsyncUsageStatus: model.AsyncUsageStatusPending,
	}).Error)
	info := &model.AsyncUsageInfo{
		RequestID:       requestID,
		RequestAt:       time.Now(),
		Status:          model.AsyncUsageStatusPending,
		GroupID:         groupID,
		TokenName:       "token-1",
		Price:           model.Price{OutputPrice: 0.5, OutputPriceUnit: 1},
		ProcessingToken: "marker-claim",
	}
	require.NoError(t, model.CreateAsyncUsageInfo(info))

	require.NoError(t, completeAsyncUsage(t.Context(), info, model.Usage{
		OutputTokens: 4,
		TotalTokens:  4,
	}, model.UsageContext{}))
	require.Equal(t, model.AsyncUsageStatusCompleted, info.Status)
	require.True(t, info.BalanceConsumed)
	require.Equal(t, 1, consumer.attempts)

	var got model.AsyncUsageInfo
	require.NoError(t, db.First(&got, info.ID).Error)
	require.Equal(t, model.AsyncUsageStatusCompleted, got.Status)
	require.True(t, got.BalanceConsumed)
	require.True(t, got.BalanceConsumeAttempted)

	oldComplete := completeClaimedAsyncUsageInfo
	completeClaimedAsyncUsageInfo = func(
		*model.AsyncUsageInfo,
		model.Usage,
		model.UsageContext,
		model.Amount,
	) (bool, error) {
		return false, errors.New("completion marker unavailable")
	}
	t.Cleanup(func() { completeClaimedAsyncUsageInfo = oldComplete })

	const parkedRequestID = "non_idempotent_completion_failure"
	require.NoError(t, db.Create(&model.Log{
		RequestID:        model.EmptyNullString(parkedRequestID),
		AsyncUsageStatus: model.AsyncUsageStatusPending,
	}).Error)
	parked := &model.AsyncUsageInfo{
		RequestID:       parkedRequestID,
		RequestAt:       time.Now(),
		Status:          model.AsyncUsageStatusPending,
		GroupID:         groupID,
		TokenName:       "token-1",
		Price:           model.Price{OutputPrice: 0.5, OutputPriceUnit: 1},
		ProcessingToken: "parked-claim",
	}
	require.NoError(t, model.CreateAsyncUsageInfo(parked))

	err = completeAsyncUsage(t.Context(), parked, model.Usage{
		OutputTokens: 4,
		TotalTokens:  4,
	}, model.UsageContext{})
	require.ErrorIs(t, err, errAsyncUsageManualSettlement)
	require.Equal(t, 2, consumer.attempts)

	var parkedReloaded model.AsyncUsageInfo
	require.NoError(t, db.First(&parkedReloaded, parked.ID).Error)
	err = completeAsyncUsage(t.Context(), &parkedReloaded, model.Usage{
		OutputTokens: 8,
		TotalTokens:  8,
	}, model.UsageContext{})
	require.ErrorIs(t, err, errAsyncUsageManualSettlement)
	require.Equal(t, 2, consumer.attempts)
	parkAsyncUsageSettlement(&parkedReloaded, err)

	var parkedGot model.AsyncUsageInfo
	require.NoError(t, db.First(&parkedGot, parked.ID).Error)
	require.Equal(t, model.AsyncUsageStatusPending, parkedGot.Status)
	require.True(t, parkedGot.BalanceConsumeAttempted)
	require.Empty(t, parkedGot.ProcessingToken)
	require.True(t, parkedGot.NextPollAt.After(time.Now().AddDate(99, 0, 0)))
}

func TestCompleteAsyncUsageRetriesPreChargeBalanceError(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.Log{},
		&model.AsyncUsageInfo{},
		&model.ConsumeError{},
	))

	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = oldLogDB
	})

	oldBalance := balance.Default
	balance.Default = preChargeFailingAsyncUsageBalance{
		err: errors.New("balance lookup unavailable"),
	}
	t.Cleanup(func() {
		balance.Default = oldBalance
	})

	require.NoError(t, model.CacheSetGroup(&model.GroupCache{
		ID:     "group-pre-charge-balance",
		Status: model.GroupStatusEnabled,
	}))
	t.Cleanup(func() {
		require.NoError(t, model.CacheDeleteGroup("group-pre-charge-balance"))
	})

	info := &model.AsyncUsageInfo{
		RequestID:       "pre_charge_balance_error",
		RequestAt:       time.Now(),
		Status:          model.AsyncUsageStatusPending,
		Model:           "gpt-5.4",
		GroupID:         "group-pre-charge-balance",
		TokenID:         1,
		TokenName:       "token-1",
		Price:           model.Price{InputPrice: 1, InputPriceUnit: 1},
		UpstreamID:      "resp_pre_charge_balance_error",
		ProcessingToken: "pre-charge-error-claim",
	}
	require.NoError(t, model.CreateAsyncUsageInfo(info))

	err = completeAsyncUsage(context.Background(), info, model.Usage{
		InputTokens: 10,
		TotalTokens: 10,
	}, model.UsageContext{})
	require.ErrorContains(t, err, "consume async usage balance before charge")
	require.Equal(t, model.AsyncUsageStatusPending, info.Status)
	require.False(t, info.BalanceConsumed)

	var consumeErrors []model.ConsumeError
	require.NoError(t, db.Find(&consumeErrors).Error)
	require.Empty(t, consumeErrors)

	var got model.AsyncUsageInfo
	require.NoError(t, db.First(&got, info.ID).Error)
	require.Equal(t, model.AsyncUsageStatusPending, got.Status)
	require.False(t, got.BalanceConsumed)
	require.Equal(t, float64(10), got.Amount.UsedAmount)
}

func TestCompleteAsyncUsageKeepsUsagePricedSuccessPendingUntilMetricsArrive(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	sqlDB, err := db.DB()
	require.NoError(t, err)
	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = oldLogDB
		require.NoError(t, sqlDB.Close())
	})

	const requestID = "late_video_metrics"
	require.NoError(t, db.Create(&model.Log{
		RequestID:        model.EmptyNullString(requestID),
		AsyncUsageStatus: model.AsyncUsageStatusPending,
	}).Error)

	info := &model.AsyncUsageInfo{
		RequestID:       requestID,
		RequestAt:       time.Now(),
		Status:          model.AsyncUsageStatusPending,
		Model:           "video-model",
		Price:           model.Price{OutputPrice: 0.5, OutputPriceUnit: 1},
		ProcessingToken: "claim-one",
	}
	require.NoError(t, model.CreateAsyncUsageInfo(info))

	err = completeAsyncUsage(t.Context(), info, model.Usage{}, model.UsageContext{})
	require.ErrorContains(t, err, "usage metrics pending")
	require.Equal(t, model.AsyncUsageStatusPending, info.Status)
	require.Zero(t, info.RetryCount)
	require.Empty(t, info.Error)

	var pending model.AsyncUsageInfo
	require.NoError(t, db.First(&pending, info.ID).Error)
	require.Equal(t, model.AsyncUsageStatusPending, pending.Status)
	require.Empty(t, pending.Error)
	require.Empty(t, pending.ProcessingToken)

	var pendingLog model.Log
	require.NoError(t, db.Where("request_id = ?", requestID).First(&pendingLog).Error)
	require.Equal(t, model.AsyncUsageStatusPending, pendingLog.AsyncUsageStatus)
	require.Zero(t, pendingLog.Amount.UsedAmount)

	require.NoError(t, db.Model(&model.AsyncUsageInfo{}).
		Where("id = ?", info.ID).
		Update("next_poll_at", time.Now().Add(-time.Second)).Error)
	require.NoError(t, db.First(&pending, info.ID).Error)
	claimed, err := model.TryClaimAsyncUsageInfo(
		&pending,
		"claim-two",
		time.Now().Add(time.Minute),
		time.Now(),
	)
	require.NoError(t, err)
	require.True(t, claimed)

	require.NoError(t, completeAsyncUsage(t.Context(), &pending, model.Usage{
		OutputTokens: 4,
		TotalTokens:  4,
	}, model.UsageContext{}))
	require.Equal(t, model.AsyncUsageStatusCompleted, pending.Status)
	require.Equal(t, 2.0, pending.Amount.UsedAmount)
}

func TestCompleteAsyncUsageKeepsPartiallyReportedVideoUsagePending(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	sqlDB, err := db.DB()
	require.NoError(t, err)
	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = oldLogDB
		require.NoError(t, sqlDB.Close())
	})

	const requestID = "partial_video_metrics"
	require.NoError(t, db.Create(&model.Log{
		RequestID:        model.EmptyNullString(requestID),
		AsyncUsageStatus: model.AsyncUsageStatusPending,
	}).Error)
	info := &model.AsyncUsageInfo{
		RequestID: requestID,
		RequestAt: time.Now(),
		Status:    model.AsyncUsageStatusPending,
		Price: model.Price{
			VideoInputPrice:     1,
			VideoInputPriceUnit: 1,
			OutputPrice:         0.5,
			OutputPriceUnit:     1,
		},
		ProcessingToken: "partial-claim",
	}
	require.NoError(t, model.CreateAsyncUsageInfo(info))

	err = completeAsyncUsage(t.Context(), info, model.Usage{
		OutputTokens: 4,
		TotalTokens:  4,
	}, model.UsageContext{})
	require.ErrorIs(t, err, errAsyncUsageMetricsPending)
	require.Equal(t, model.AsyncUsageStatusPending, info.Status)
	require.Zero(t, info.Amount.UsedAmount)
}

func TestCompleteAsyncUsageReplaysAmbiguousIdempotentDebitExactlyOnce(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.Log{},
		&model.AsyncUsageInfo{},
		&model.ConsumeError{},
	))

	sqlDB, err := db.DB()
	require.NoError(t, err)
	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = oldLogDB
		require.NoError(t, sqlDB.Close())
	})

	const (
		requestID = "ambiguous_idempotent_debit"
		groupID   = "group-ambiguous-idempotent-debit"
	)
	require.NoError(t, db.Create(&model.Log{
		RequestID:        model.EmptyNullString(requestID),
		AsyncUsageStatus: model.AsyncUsageStatusPending,
	}).Error)
	require.NoError(t, model.CacheSetGroup(&model.GroupCache{
		ID:     groupID,
		Status: model.GroupStatusEnabled,
	}))
	t.Cleanup(func() { require.NoError(t, model.CacheDeleteGroup(groupID)) })

	consumer := &replaySafeAmbiguousAsyncUsageConsumer{charges: map[string]int{}}
	oldBalance := balance.Default
	balance.Default = replaySafeAmbiguousAsyncUsageBalance{consumer: consumer}
	t.Cleanup(func() { balance.Default = oldBalance })

	info := &model.AsyncUsageInfo{
		RequestID:       requestID,
		RequestAt:       time.Now(),
		Status:          model.AsyncUsageStatusPending,
		Model:           "video-model",
		GroupID:         groupID,
		TokenID:         1,
		TokenName:       "playground",
		Price:           model.Price{OutputPrice: 0.5, OutputPriceUnit: 1},
		ProcessingToken: "claim-token",
	}
	require.NoError(t, model.CreateAsyncUsageInfo(info))
	usage := model.Usage{OutputTokens: 4, TotalTokens: 4}

	err = completeAsyncUsage(t.Context(), info, usage, model.UsageContext{})
	require.ErrorContains(t, err, "wallet response lost")
	require.ErrorIs(t, err, errAsyncUsageSettlementPending)
	require.Equal(t, model.AsyncUsageStatusPending, info.Status)
	require.False(t, info.BalanceConsumed)
	require.Equal(t, 1, consumer.charges[requestID])
	require.Equal(t, 2.0, info.Amount.UsedAmount)
	var prepared model.AsyncUsageInfo
	require.NoError(t, db.First(&prepared, info.ID).Error)
	require.Equal(t, int64(4), int64(prepared.Usage.OutputTokens))
	require.Equal(t, 2.0, prepared.Amount.UsedAmount)

	require.NoError(t, completeAsyncUsage(t.Context(), info, model.Usage{
		OutputTokens: 10,
		TotalTokens:  10,
	}, model.UsageContext{}))
	require.Equal(t, 2, consumer.attempts)
	require.Equal(t, 1, consumer.charges[requestID])
	require.Equal(t, []float64{2, 2}, consumer.amounts)
	require.True(t, info.BalanceConsumed)
	require.Equal(t, model.AsyncUsageStatusCompleted, info.Status)

	var consumeErrors []model.ConsumeError
	require.NoError(t, db.Find(&consumeErrors).Error)
	require.Empty(t, consumeErrors)
}

func TestCompleteAsyncUsagePreservesStoredPriceCondition(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = oldLogDB
	})

	requestID := "async_condition"
	require.NoError(t, db.Create(&model.Log{
		RequestID:        model.EmptyNullString(requestID),
		AsyncUsageStatus: model.AsyncUsageStatusPending,
	}).Error)

	info := &model.AsyncUsageInfo{
		RequestID: requestID,
		RequestAt: time.Now(),
		Status:    model.AsyncUsageStatusPending,
		Model:     "video-model",
		Price: model.Price{
			OutputPrice:     0.1,
			OutputPriceUnit: 1,
			ConditionalPrices: []model.ConditionalPrice{
				{
					Condition: model.PriceCondition{Resolution: []string{"720p"}},
					Price: model.Price{
						OutputPrice:     0.4,
						OutputPriceUnit: 1,
					},
				},
			},
		},
		UsageContext: model.UsageContext{
			Resolution: "720P",
		},
		ProcessingToken: "claim-token",
	}
	require.NoError(t, model.CreateAsyncUsageInfo(info))

	require.NoError(t, completeAsyncUsage(context.Background(), info, model.Usage{
		OutputTokens: 5,
		TotalTokens:  5,
	}, model.UsageContext{}))
	require.Equal(t, model.AsyncUsageStatusCompleted, info.Status)
	require.Equal(t, "720P", info.UsageContext.Resolution)
	require.Equal(t, 2.0, info.Amount.UsedAmount)

	var got model.Log
	require.NoError(t, db.Where("request_id = ?", requestID).First(&got).Error)
	require.Equal(t, "720P", got.UsageContext.Resolution)
	require.Equal(t, model.ZeroNullFloat64(0.4), got.Price.OutputPrice)
	require.Equal(t, model.ZeroNullInt64(1), got.Price.OutputPriceUnit)
	require.Empty(t, got.Price.ConditionalPrices)
	require.Equal(t, 2.0, got.Amount.UsedAmount)
}

func TestCompleteAsyncUsageUsesOriginalRequestTimeForDailyPrice(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = oldLogDB
	})

	location, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)

	requestID := "async_daily_time"
	require.NoError(t, db.Create(&model.Log{
		RequestID:        model.EmptyNullString(requestID),
		AsyncUsageStatus: model.AsyncUsageStatusPending,
	}).Error)

	info := &model.AsyncUsageInfo{
		RequestID: requestID,
		RequestAt: time.Date(2026, time.July, 20, 10, 0, 0, 0, location),
		Status:    model.AsyncUsageStatusPending,
		Model:     "video-model",
		Price: model.Price{
			OutputPrice:     0.1,
			OutputPriceUnit: 1,
			ConditionalPrices: []model.ConditionalPrice{
				{
					Condition: model.PriceCondition{
						DailyStartTime: "09:00",
						DailyEndTime:   "12:00",
						Timezone:       "Asia/Shanghai",
					},
					Price: model.Price{OutputPrice: 0.4, OutputPriceUnit: 1},
				},
			},
		},
		ProcessingToken: "claim-token",
	}
	require.NoError(t, model.CreateAsyncUsageInfo(info))

	require.NoError(t, completeAsyncUsage(context.Background(), info, model.Usage{
		OutputTokens: 5,
		TotalTokens:  5,
	}, model.UsageContext{}))
	require.Equal(t, 2.0, info.Amount.UsedAmount)

	var got model.Log
	require.NoError(t, db.Where("request_id = ?", requestID).First(&got).Error)
	require.Equal(t, model.ZeroNullFloat64(0.4), got.Price.OutputPrice)
	require.Equal(t, 2.0, got.Amount.UsedAmount)
}

func TestCompleteAsyncUsageChargesStoredPerRequestPrice(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = oldLogDB
	})

	requestID := "async_per_request"
	require.NoError(t, db.Create(&model.Log{
		RequestID:        model.EmptyNullString(requestID),
		AsyncUsageStatus: model.AsyncUsageStatusPending,
	}).Error)

	info := &model.AsyncUsageInfo{
		RequestID:       requestID,
		RequestAt:       time.Now(),
		Status:          model.AsyncUsageStatusPending,
		Model:           "video-model",
		Price:           model.Price{PerRequestPrice: 0.25, OutputPrice: 0.5, OutputPriceUnit: 1},
		ProcessingToken: "claim-token",
	}
	require.NoError(t, model.CreateAsyncUsageInfo(info))

	require.NoError(t, completeAsyncUsage(
		context.Background(),
		info,
		model.Usage{},
		model.UsageContext{},
	))
	require.Equal(t, model.AsyncUsageStatusCompleted, info.Status)
	require.Equal(t, 0.25, info.Amount.UsedAmount)

	var got model.Log
	require.NoError(t, db.Where("request_id = ?", requestID).First(&got).Error)
	require.Equal(t, 0.25, got.Amount.UsedAmount)
}

func TestCompleteAsyncUsagePersistsBalanceConsumed(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = oldLogDB
	})

	info := &model.AsyncUsageInfo{
		RequestID:       "async_balance_consumed",
		RequestAt:       time.Now(),
		Status:          model.AsyncUsageStatusPending,
		Model:           "video-model",
		BalanceConsumed: true,
		ProcessingToken: "claim-token",
	}
	require.NoError(t, model.CreateAsyncUsageInfo(info))

	require.NoError(t, completeAsyncUsage(context.Background(), info, model.Usage{
		OutputTokens: 4,
		TotalTokens:  4,
	}, model.UsageContext{}))
	require.Equal(t, model.AsyncUsageStatusCompleted, info.Status)
	require.True(t, info.BalanceConsumed)

	var got model.AsyncUsageInfo
	require.NoError(t, db.First(&got, info.ID).Error)
	require.True(t, got.BalanceConsumed)
	require.Equal(t, model.AsyncUsageStatusCompleted, got.Status)
}

func TestTouchAsyncUsagePollCursorAdvancesUpdatedAtAndNextPollAt(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.AsyncUsageInfo{}))

	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = oldLogDB
	})

	oldUpdatedAt := time.Now().Add(-time.Hour)
	oldNextPollAt := time.Now().Add(-time.Minute)
	info := &model.AsyncUsageInfo{
		RequestID:       "pending",
		Status:          model.AsyncUsageStatusPending,
		UpdatedAt:       oldUpdatedAt,
		NextPollAt:      oldNextPollAt,
		Error:           "previous error",
		ProcessingToken: "claim-token",
	}
	require.NoError(t, db.Create(info).Error)

	beforeTouch := time.Now()

	touchAsyncUsagePollCursor(info)

	var got model.AsyncUsageInfo
	require.NoError(t, db.First(&got, info.ID).Error)
	require.True(t, got.UpdatedAt.After(oldUpdatedAt))
	require.True(t, got.NextPollAt.After(beforeTouch))
	require.Empty(t, got.Error)
	require.Empty(t, got.ProcessingToken)
}

func TestScheduleAsyncUsageSettlementRetryDoesNotFailSuccessfulJobAtRetryLimit(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.AsyncUsageInfo{}))

	sqlDB, err := db.DB()
	require.NoError(t, err)
	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = oldLogDB
		require.NoError(t, sqlDB.Close())
	})

	info := &model.AsyncUsageInfo{
		RequestID:       "settlement_retry_limit",
		Status:          model.AsyncUsageStatusPending,
		RetryCount:      asyncUsageMaxRetry - 1,
		Error:           "previous wallet error",
		ProcessingToken: "claim-token",
	}
	require.NoError(t, db.Create(info).Error)

	beforeRetry := time.Now()
	scheduleAsyncUsageSettlementRetry(info)

	require.Equal(t, asyncUsageMaxRetry-1, info.RetryCount)
	require.Equal(t, model.AsyncUsageStatusPending, info.Status)
	require.Empty(t, info.Error)
	require.WithinDuration(
		t,
		beforeRetry.Add(model.AsyncUsageMaxPollDelay),
		info.NextPollAt,
		time.Second,
	)

	var got model.AsyncUsageInfo
	require.NoError(t, db.First(&got, info.ID).Error)
	require.Equal(t, model.AsyncUsageStatusPending, got.Status)
	require.Equal(t, asyncUsageMaxRetry-1, got.RetryCount)
	require.Empty(t, got.Error)
	require.Empty(t, got.ProcessingToken)
}

func TestAsyncUsageClaimRenewalCancelsWorkOnLostClaim(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.AsyncUsageInfo{}))

	sqlDB, err := db.DB()
	require.NoError(t, err)
	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = oldLogDB
		require.NoError(t, sqlDB.Close())
	})

	info := &model.AsyncUsageInfo{
		RequestID:       "lost_claim",
		Status:          model.AsyncUsageStatusPending,
		ProcessingToken: "stale-token",
	}
	require.NoError(t, db.Create(info).Error)
	require.NoError(t, db.Model(&model.AsyncUsageInfo{}).
		Where("id = ?", info.ID).
		Update("processing_token", "new-owner").Error)

	workCtx, stop := startAsyncUsageClaimRenewalAtInterval(
		t.Context(),
		info,
		time.Millisecond,
	)
	defer stop()

	select {
	case <-workCtx.Done():
		require.ErrorIs(t, workCtx.Err(), context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("lost claim did not cancel in-flight work")
	}
}

func TestCompleteAsyncUsageDoesNotDebitAfterClaimCancellation(t *testing.T) {
	consumer := &replaySafeAmbiguousAsyncUsageConsumer{charges: map[string]int{}}
	oldBalance := balance.Default
	balance.Default = replaySafeAmbiguousAsyncUsageBalance{consumer: consumer}
	t.Cleanup(func() { balance.Default = oldBalance })

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := completeAsyncUsage(ctx, &model.AsyncUsageInfo{
		RequestID: "cancelled_settlement",
		GroupID:   "group-cancelled-settlement",
		Price:     model.Price{OutputPrice: 0.5, OutputPriceUnit: 1},
	}, model.Usage{OutputTokens: 4, TotalTokens: 4}, model.UsageContext{})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, consumer.attempts)
}

func TestMarkAsyncUsageFailedWritesLogMessage(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = oldLogDB
	})

	requestID := "async_fail_log"
	require.NoError(t, db.Create(&model.Log{
		RequestID:        model.EmptyNullString(requestID),
		AsyncUsageStatus: model.AsyncUsageStatusPending,
	}).Error)

	info := &model.AsyncUsageInfo{
		RequestID:       requestID,
		Status:          model.AsyncUsageStatusPending,
		ProcessingToken: "claim-token",
	}
	require.NoError(t, db.Create(info).Error)

	markAsyncUsageFailed(info, "upstream task failed")

	var got model.Log
	require.NoError(t, db.Where("request_id = ?", requestID).First(&got).Error)
	require.Equal(t, model.AsyncUsageStatusFailed, got.AsyncUsageStatus)
	require.Equal(t, "upstream task failed", string(got.Content))
}
