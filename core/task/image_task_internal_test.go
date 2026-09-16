package task

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestImageWorkerPersistsResultBeforeSettlement(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "worker.db"))
	require.NoError(t, err)
	require.NoError(
		t,
		db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}, &model.Channel{}),
	)

	oldDB, oldLog := model.DB, model.LogDB
	model.DB, model.LogDB = db, db
	t.Cleanup(func() { model.DB, model.LogDB = oldDB, oldLog })

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fal-ai/minimax/requests/upstream/status" {
			if _, writeErr := w.Write([]byte(`{"status":"COMPLETED"}`)); writeErr != nil {
				t.Errorf("write mock response: %v", writeErr)
			}
		} else {
			if _, writeErr := w.Write(
				[]byte(`{"images":[{"url":"https://cdn.example/a.png"}]}`),
			); writeErr != nil {
				t.Errorf("write mock response: %v", writeErr)
			}
		}
	}))
	defer s.Close()

	ch := &model.Channel{Type: model.ChannelTypeFal, Key: "secret"}
	require.NoError(t, db.Create(ch).Error)
	info := &model.AsyncUsageInfo{
		RequestID: "durable",
		ChannelID: ch.ID,
		BaseURL:   s.URL,
		GroupID:   "g",
		TokenID:   1,
	}
	_, _, err = model.ReserveImageTask(
		&model.ImageTask{
			ID:            "durable",
			GroupID:       "g",
			TokenID:       1,
			UpstreamModel: "fal-ai/minimax/image-01",
		},
		info,
	)
	require.NoError(t, err)
	require.NoError(t, model.AcceptImageTask("durable", "upstream"))
	// Simulate a different process by throwing away the request's in-memory objects.
	info = &model.AsyncUsageInfo{}
	require.NoError(t, db.First(info).Error)
	claimed, err := claimAsyncUsage(info)
	require.NoError(t, err)
	require.True(t, claimed)
	processOneImageUsage(t.Context(), info)

	result, err := model.GetImageTask("durable", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "completed", result.Status)
	require.Len(t, result.Data, 1)

	var saved model.AsyncUsageInfo
	require.NoError(t, db.First(&saved).Error)
	require.Equal(t, model.AsyncUsageStatusCompleted, saved.Status)
	claimed, err = claimAsyncUsage(&saved)
	require.NoError(t, err)
	require.False(t, claimed)
}

type imageLogReplayConsumer struct {
	replaySafeAmbiguousAsyncUsageConsumer
	t *testing.T
}

func (c *imageLogReplayConsumer) PostGroupConsume(
	ctx context.Context,
	token string,
	amount float64,
) (float64, error) {
	var entry model.Log
	require.NoError(
		c.t,
		model.LogDB.Where("request_id = ?", balance.RequestIDFromContext(ctx)).First(&entry).Error,
	)
	require.EqualValues(c.t, 1, entry.Usage.ImageOutputTokens)
	require.InDelta(c.t, 0.25, entry.Amount.UsedAmount, 0.000001)
	require.Equal(c.t, "USD", entry.Currency)
	require.Equal(c.t, "release-1", entry.PricingVersion)

	currency, version := balance.PricingFromContext(ctx)
	require.Equal(c.t, "USD", currency)
	require.Equal(c.t, "release-1", version)

	return c.replaySafeAmbiguousAsyncUsageConsumer.PostGroupConsume(ctx, token, amount)
}

type imageLogReplayBalance struct{ consumer *imageLogReplayConsumer }

func (b imageLogReplayBalance) GetGroupRemainBalance(
	context.Context,
	model.GroupCache,
) (float64, balance.PostGroupConsumer, error) {
	return 100, b.consumer, nil
}

func (imageLogReplayBalance) GetGroupQuota(
	context.Context,
	model.GroupCache,
) (*balance.GroupQuota, error) {
	return &balance.GroupQuota{Total: 100, Remain: 100}, nil
}

func TestImageWorkerPaidSettlementReplayUsesFinalLog(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "paid.db"))
	require.NoError(t, err)
	require.NoError(
		t,
		db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}, &model.Channel{}),
	)

	oldDB, oldLog, oldBalance := model.DB, model.LogDB, balance.Default
	model.DB, model.LogDB = db, db
	t.Cleanup(func() { model.DB, model.LogDB, balance.Default = oldDB, oldLog, oldBalance })
	consumer := &imageLogReplayConsumer{
		replaySafeAmbiguousAsyncUsageConsumer: replaySafeAmbiguousAsyncUsageConsumer{
			charges: map[string]int{},
		},
		t: t,
	}
	balance.Default = imageLogReplayBalance{consumer}

	require.NoError(
		t,
		model.CacheSetGroup(&model.GroupCache{ID: "image-paid", Status: model.GroupStatusEnabled}),
	)
	t.Cleanup(func() { require.NoError(t, model.CacheDeleteGroup("image-paid")) })

	polls := 0

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++

		if strings.HasSuffix(r.URL.Path, "/status") {
			if _, writeErr := w.Write([]byte(`{"status":"COMPLETED"}`)); writeErr != nil {
				t.Errorf("write mock response: %v", writeErr)
			}
		} else {
			if _, writeErr := w.Write(
				[]byte(`{"images":[{"url":"https://cdn.example/paid.png"}]}`),
			); writeErr != nil {
				t.Errorf("write mock response: %v", writeErr)
			}
		}
	}))
	defer s.Close()

	ch := &model.Channel{Type: model.ChannelTypeFal, Key: "key"}
	require.NoError(t, db.Create(ch).Error)
	info := &model.AsyncUsageInfo{
		RequestID:       "paid-image",
		GroupID:         "image-paid",
		TokenID:         1,
		ChannelID:       ch.ID,
		BaseURL:         s.URL,
		PricingCurrency: "USD",
		PricingVersion:  "release-1",
		Price:           model.Price{ImageOutputPrice: 0.25, ImageOutputPriceUnit: 1},
	}
	_, _, err = model.ReserveImageTask(
		&model.ImageTask{
			ID:             "paid-image",
			GroupID:        "image-paid",
			TokenID:        1,
			UpstreamModel:  "fal-ai/minimax/image-01",
			ExpectedImages: 1,
			ChannelType:    model.ChannelTypeFal,
			KeyFingerprint: model.ImageChannelKeyFingerprint(ch.Key),
		},
		info,
	)
	require.NoError(t, err)
	require.NoError(t, model.AcceptImageTask("paid-image", "upstream"))
	// Even an out-of-band credential edit must not send the old job to a new account.
	require.NoError(t, db.Model(ch).Update("key", "different-account").Error)

	var interrupted model.AsyncUsageInfo
	require.NoError(t, db.First(&interrupted).Error)
	claimed, claimErr := claimAsyncUsage(&interrupted)
	require.NoError(t, claimErr)
	require.True(t, claimed)
	processOneImageUsage(t.Context(), &interrupted)
	require.Zero(t, polls)
	require.Zero(t, consumer.attempts)
	require.NoError(t, db.Model(ch).Update("key", "key").Error)

	for range 2 {
		var fresh model.AsyncUsageInfo
		require.NoError(t, db.First(&fresh).Error)
		require.NoError(
			t,
			db.Model(&fresh).Update("next_poll_at", time.Now().Add(-time.Second)).Error,
		)
		fresh.NextPollAt = time.Now().Add(-time.Second)
		claimed, err := claimAsyncUsage(&fresh)
		require.NoError(t, err)
		require.True(t, claimed)
		processOneImageUsage(t.Context(), &fresh)
	}

	require.Equal(t, 2, consumer.attempts)
	require.Equal(t, 1, consumer.charges["paid-image"])
	require.Equal(t, 2, polls, "retry must use stored result, no further upstream calls")

	var final model.AsyncUsageInfo
	require.NoError(t, db.First(&final).Error)
	require.Equal(t, model.AsyncUsageStatusCompleted, final.Status)
	require.True(t, final.BalanceConsumed)
}
