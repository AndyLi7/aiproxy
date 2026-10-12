package task

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/notify"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// attributionWallet is an external wallet that records consume and
// measured-zero bodies.
type attributionWallet struct {
	mu            sync.Mutex
	consumes      []map[string]any
	zeros         []map[string]any
	zeroLogStatus []model.AsyncUsageStatus
	failZero      bool
	db            *gorm.DB
}

func newAttributionWallet(t *testing.T, db *gorm.DB) *attributionWallet {
	t.Helper()

	w := &attributionWallet{db: db}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil && r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}

		w.mu.Lock()
		defer w.mu.Unlock()

		switch r.URL.Path {
		case "/api/internal/wallet/balance":
			_, _ = rw.Write([]byte(`{"code":0,"data":{"balance":100}}`))
		case "/api/internal/wallet/consume":
			w.consumes = append(w.consumes, body)
			_, _ = rw.Write([]byte(`{"code":0,"data":{"charged":1}}`))
		case "/api/internal/wallet/measured-zero":
			w.zeros = append(w.zeros, body)
			// The outbox reports the receipt only after the call row is settled.
			var row model.Log
			if requestID, _ := body["requestId"].(string); requestID != "" &&
				w.db.Where("request_id = ?", requestID).First(&row).Error == nil {
				w.zeroLogStatus = append(w.zeroLogStatus, row.AsyncUsageStatus)
			}

			if w.failZero {
				_, _ = rw.Write([]byte(`{"code":-1,"message":"wallet unavailable"}`))
				return
			}

			_, _ = rw.Write([]byte(`{"code":0,"data":{"receipt":"created"}}`))
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(server.Close)

	old := balance.Default
	balance.Default = balance.NewExternalHTTP(server.URL, "local-only")
	t.Cleanup(func() { balance.Default = old })

	return w
}

type capturedAlerts struct {
	mu   sync.Mutex
	keys []string
}

func (c *capturedAlerts) Notify(notify.Level, string, string) {}

func (c *capturedAlerts) NotifyThrottle(_ notify.Level, key string, _ time.Duration, _, _ string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.keys = append(c.keys, key)
}

func (c *capturedAlerts) has(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, got := range c.keys {
		if got == "notifylimit:error:"+key {
			return true
		}
	}

	return false
}

func captureAlerts(t *testing.T) *capturedAlerts {
	t.Helper()

	alerts := &capturedAlerts{}
	notify.SetDefaultNotifier(alerts)
	t.Cleanup(func() { notify.SetDefaultNotifier(&notify.StdNotifier{}) })

	return alerts
}

func attributionDB(t *testing.T, groupID string) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}, &model.ConsumeError{}))

	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = oldLogDB })

	require.NoError(t, model.CacheSetGroup(&model.GroupCache{ID: groupID, Status: model.GroupStatusEnabled}))
	t.Cleanup(func() { require.NoError(t, model.CacheDeleteGroup(groupID)) })

	return db
}

var attributionAt = time.Date(2026, 10, 12, 1, 2, 3, 456_000_000, time.UTC)

func p2Identity() model.UsageIdentity {
	return model.NewUsageIdentity(7, "vendor/model", "text-to-video", model.RequestSourcePlayground, attributionAt)
}

func requireBodyIdentity(t *testing.T, body map[string]any, op string) {
	t.Helper()
	require.Equal(t, op, body["billingOperationId"])
	require.EqualValues(t, 7, body["apiKeyId"])
	require.Equal(t, "vendor/model/text-to-video", body["usageModelId"])
	require.Equal(t, model.RequestSourcePlayground, body["source"])
	require.EqualValues(t, attributionAt.UnixMilli(), body["usageAtMs"])
}

// The outbox's consume carries the attribution stored on the info (video,
// measured image, unprepaid image task); an info created before P2 sends none.
func TestOutboxConsumeCarriesInfoIdentity(t *testing.T) {
	const groupID = "u_outbox_identity"

	attributionDB(t, groupID)
	wallet := newAttributionWallet(t, model.LogDB)

	for _, info := range []*model.AsyncUsageInfo{
		{RequestID: "video", BillingOperationID: "op-video", Mode: int(mode.VideoGenerationsJobs)},
		{RequestID: "measured", BillingOperationID: "op-measured", MeasuredImage: true, LogID: 1},
		{RequestID: "image-task", BillingOperationID: "op-image", ImageTaskID: "image-task", LogID: 2},
	} {
		info.GroupID, info.TokenID, info.TokenName = groupID, 7, "prod"
		info.SetUsageIdentity(p2Identity())

		charged, replaySafe, err := consumeAsyncUsageGroupBalance(t.Context(), info, 1)
		require.NoError(t, err)
		require.True(t, charged)
		require.True(t, replaySafe)
	}

	_, _, err := consumeAsyncUsageGroupBalance(t.Context(), &model.AsyncUsageInfo{
		RequestID: "pre-p2", BillingOperationID: "op-pre", GroupID: groupID, TokenID: 7, TokenName: "prod",
	}, 1)
	require.NoError(t, err)

	require.Len(t, wallet.consumes, 4)
	requireBodyIdentity(t, wallet.consumes[0], "op-video")
	requireBodyIdentity(t, wallet.consumes[1], "op-measured")
	requireBodyIdentity(t, wallet.consumes[2], "op-image")
	require.Equal(t, "op-pre", wallet.consumes[3]["billingOperationId"])

	for _, key := range []string{"apiKeyId", "usageModelId", "source", "usageAtMs"} {
		require.NotContains(t, wallet.consumes[3], key, "a pre-P2 info has no attribution")
	}
}

func measuredZeroOutbox(t *testing.T, db *gorm.DB, groupID, requestID string, requestAt time.Time) *model.AsyncUsageInfo {
	t.Helper()

	zero := int64(0)
	info := &model.AsyncUsageInfo{
		RequestID: requestID, BillingOperationID: "op-" + requestID, RequestAt: requestAt,
		GroupID: groupID, TokenID: 7, TokenName: "prod", PricingVersion: "v9",
		Mode: int(mode.ImagesGenerations), Model: "image",
		Price: model.Price{ImageBilling: &model.ImageBillingPolicy{Version: 1, Scenario: "generation"}},
		Amount: model.Amount{ImageBillingResult: &model.ImageBillingResult{
			State: "complete", AmountMicros: &zero, Lines: []model.ImageBillingLine{},
		}},
	}
	info.SetUsageIdentity(p2Identity())
	entry := &model.Log{
		RequestID: model.EmptyNullString(requestID), RequestAt: requestAt, GroupID: groupID, TokenID: 7,
		Mode: int(mode.ImagesGenerations), Code: http.StatusOK, BillingOperationID: model.EmptyNullString(info.BillingOperationID),
	}
	require.NoError(t, model.CreateMeasuredImageSettlement(entry, info))
	require.Equal(t, model.AsyncUsageStatusPending, info.Status, "a complete zero bill is claimable (C3)")

	claimed, err := model.TryClaimAsyncUsageInfo(info, "claim-"+requestID, time.Now().Add(time.Minute), time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)

	return info
}

// Key usage P2 (§4.4, C3): the outbox settles a complete zero measured bill by
// asking the wallet for a measured-zero receipt with the call's op and
// attribution, after the call row is settled. Old: the info was stored
// Completed and never claimed; the receipt waited for a customer read.
func TestMeasuredZeroOutboxReportsReceipt(t *testing.T) {
	const groupID = "u_measured_zero"

	db := attributionDB(t, groupID)
	wallet := newAttributionWallet(t, db)
	alerts := captureAlerts(t)

	info := measuredZeroOutbox(t, db, groupID, "zero-ok", time.Now())
	processOneAsyncUsage(t.Context(), info)

	require.Len(t, wallet.zeros, 1)
	requireBodyIdentity(t, wallet.zeros[0], "op-zero-ok")
	require.Equal(t, groupID, wallet.zeros[0]["group"])
	require.Equal(t, "zero-ok", wallet.zeros[0]["requestId"])
	require.Equal(t, "v9", wallet.zeros[0]["pricingVersion"])
	require.Equal(t, []model.AsyncUsageStatus{model.AsyncUsageStatusCompleted}, wallet.zeroLogStatus,
		"the call row is settled (async 2) before the receipt is requested")
	require.Empty(t, wallet.consumes, "a zero bill is never debited")

	var got model.AsyncUsageInfo
	require.NoError(t, db.First(&got, info.ID).Error)
	require.Equal(t, model.AsyncUsageStatusCompleted, got.Status)

	// A wallet error keeps the outbox pending and retries in 3 minutes.
	wallet.failZero = true
	failing := measuredZeroOutbox(t, db, groupID, "zero-retry", time.Now())
	processOneAsyncUsage(t.Context(), failing)

	got = model.AsyncUsageInfo{}
	require.NoError(t, db.First(&got, failing.ID).Error)
	require.Equal(t, model.AsyncUsageStatusPending, got.Status)
	require.Empty(t, got.ProcessingToken)
	require.WithinDuration(t, time.Now().Add(model.AsyncUsageMaxPollDelay), got.NextPollAt, 10*time.Second)
	require.False(t, alerts.has("asyncUsageMeasuredZero"))

	// 24 hours after the request it completes with an alert; the catch-up and
	// the invariant cover the missing receipt.
	expired := measuredZeroOutbox(t, db, groupID, "zero-expired", time.Now().Add(-measuredZeroReceiptWindow-time.Hour))
	processOneAsyncUsage(t.Context(), expired)

	got = model.AsyncUsageInfo{}
	require.NoError(t, db.First(&got, expired.ID).Error)
	require.Equal(t, model.AsyncUsageStatusCompleted, got.Status)
	require.True(t, alerts.has("asyncUsageMeasuredZero"))
	require.Empty(t, wallet.consumes)
}

// Plan C5: a video outbox whose call row is missing waits up to 15 minutes for
// the row the same request writes, without charging; then it charges as
// before and alerts. Old: it charged at once and the row stayed processing.
func TestVideoMissingLogWaitsThenCharges(t *testing.T) {
	const groupID = "u_video_missing_log"

	db := attributionDB(t, groupID)
	wallet := newAttributionWallet(t, db)
	alerts := captureAlerts(t)

	info := &model.AsyncUsageInfo{
		RequestID: "video-missing", BillingOperationID: "op-video-missing", RequestAt: time.Now(),
		GroupID: groupID, TokenID: 7, TokenName: "prod", Mode: int(mode.VideoGenerationsJobs),
		Price:           model.Price{PerRequestPrice: 1},
		UpstreamID:      "upstream-video",
		ProcessingToken: "claim-video",
	}
	info.SetUsageIdentity(p2Identity())
	require.NoError(t, model.CreateAsyncUsageInfo(info))

	err := completeAsyncUsage(t.Context(), info, model.Usage{}, model.UsageContext{})
	require.ErrorIs(t, err, errAsyncUsageSettlementPending)
	require.Empty(t, wallet.consumes, "no debit while the call row may still arrive")
	require.False(t, alerts.has("asyncUsageLogMissing"))

	// The row arrives: the retry settles it and charges once.
	require.NoError(t, db.Create(&model.Log{
		RequestID: "video-missing", GroupID: groupID, TokenID: 7, Mode: int(mode.VideoGenerationsJobs),
		Code: 200, AsyncUsageStatus: model.AsyncUsageStatusPending, BillingOperationID: "op-video-missing",
	}).Error)
	require.NoError(t, completeAsyncUsage(t.Context(), info, model.Usage{}, model.UsageContext{}))
	require.Len(t, wallet.consumes, 1)
	requireBodyIdentity(t, wallet.consumes[0], "op-video-missing")

	var row model.Log
	require.NoError(t, db.Where("request_id = ?", "video-missing").First(&row).Error)
	require.Equal(t, model.AsyncUsageStatusCompleted, row.AsyncUsageStatus)

	// A row that never arrives: after the grace the outbox charges and alerts.
	late := &model.AsyncUsageInfo{
		RequestID: "video-never", BillingOperationID: "op-video-never", RequestAt: time.Now(),
		GroupID: groupID, TokenID: 7, TokenName: "prod", Mode: int(mode.VideoGenerationsJobs),
		Price:           model.Price{PerRequestPrice: 1},
		UpstreamID:      "upstream-never",
		ProcessingToken: "claim-never",
	}
	late.SetUsageIdentity(p2Identity())
	require.NoError(t, model.CreateAsyncUsageInfo(late))

	require.ErrorIs(t, completeAsyncUsage(t.Context(), late, model.Usage{}, model.UsageContext{}), errAsyncUsageSettlementPending)
	require.Len(t, wallet.consumes, 1)

	late.CreatedAt = time.Now().Add(-asyncUsageLogMissingGrace - time.Second)
	require.NoError(t, completeAsyncUsage(t.Context(), late, model.Usage{}, model.UsageContext{}))
	require.Len(t, wallet.consumes, 2)
	require.Equal(t, "op-video-never", wallet.consumes[1]["billingOperationId"])
	require.True(t, alerts.has("asyncUsageLogMissing"))
	require.True(t, late.BalanceConsumed)

	var completed model.AsyncUsageInfo
	require.NoError(t, db.First(&completed, late.ID).Error)
	require.Equal(t, model.AsyncUsageStatusCompleted, completed.Status)

	var rows int64
	require.NoError(t, db.Model(&model.Log{}).Where("request_id = ?", "video-never").Count(&rows).Error)
	require.Zero(t, rows, "no row is touched or invented")
}

// Plan C8: an unprepaid image task's outbox prices at info.request_at (the
// handler instant), never at usage_at_ms. The window below starts between the
// two, so pricing at the usage time would charge 1 instead of 5.
func TestImageTaskOutboxPricesAtHandlerTime(t *testing.T) {
	const groupID = "u_image_pricing"

	db := attributionDB(t, groupID)
	wallet := newAttributionWallet(t, db)

	windowStart := time.Now().Truncate(time.Second)
	usage := model.NewUsageIdentity(7, "vendor/image", "text-to-image", model.RequestSourceAPI, windowStart.Add(-2*time.Second))

	row := &model.Log{
		RequestID: "image-pricing", GroupID: groupID, TokenID: 7, Code: http.StatusAccepted,
		Mode: int(mode.ImagesGenerations), AsyncUsageStatus: model.AsyncUsageStatusPending,
		BillingOperationID: "op-image-pricing",
	}
	require.NoError(t, db.Create(row).Error)

	info := &model.AsyncUsageInfo{
		RequestID: "image-pricing", BillingOperationID: "op-image-pricing", ImageTaskID: "image-pricing",
		LogID: row.ID, RequestAt: windowStart.Add(time.Second), GroupID: groupID, TokenID: 7, TokenName: "prod",
		Mode: int(mode.ImagesGenerations), ProcessingToken: "claim-image-pricing",
		Price: model.Price{PerRequestPrice: 1, ConditionalPrices: []model.ConditionalPrice{{
			Condition: model.PriceCondition{StartTime: windowStart.Unix()},
			Price:     model.Price{PerRequestPrice: 5},
		}}},
	}
	info.SetUsageIdentity(usage)
	require.NoError(t, model.CreateAsyncUsageInfo(info))

	require.NoError(t, completeAsyncUsage(t.Context(), info, model.Usage{ImageOutputTokens: 1}, model.UsageContext{}))
	require.Len(t, wallet.consumes, 1)
	require.InDelta(t, 5, wallet.consumes[0]["amount"], 1e-9)
	require.EqualValues(t, usage.UsageAtMs(), wallet.consumes[0]["usageAtMs"], "attribution keeps the request start")
}
