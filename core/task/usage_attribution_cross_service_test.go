package task

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/consume"
	"github.com/labring/aiproxy/core/common/imageprepayment"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

const (
	crossServiceWalletKey = "local-d34-test-only"
	crossServiceGroup     = "u_u"
)

type rejectingNativeProvider struct{}

func (rejectingNativeProvider) SubmitNative(context.Context, string, []byte) (string, error) {
	return "", adaptor.ErrImageSubmissionRejected
}

// crossServiceLedgerRow is one wallet_ledger row of the disposable
// application wallet (SQLite: usage_at in Unix milliseconds).
type crossServiceLedgerRow struct {
	RequestID   string  `json:"request_id"`
	Amount      int64   `json:"amount_micros"`
	OperationID *string `json:"operation_id"`
	APIKeyID    *int64  `json:"api_key_id"`
	ModelID     *string `json:"model_id"`
	Source      *string `json:"source"`
	UsageAt     *int64  `json:"usage_at"`
}

// Key usage P2 §6.3, across the real application wallet (disposable SQLite,
// tests/fixtures/prepayment-wallet-server.ts) and this gateway's code: for a
// native submission and its settlement, a prepaid image task admitted and
// refunded, a relay consume and a video outbox consume, every ledger row of an
// operation carries the identity of its call row and −Σamount equals the final
// charge. Run by scripts/test-native-cross-service.mjs (package ./task).
func TestUsageAttributionLedgerMatchesLogs(t *testing.T) {
	walletURL := os.Getenv("D34_TEST_WALLET_URL")
	if walletURL == "" {
		t.Skip("requires the disposable application wallet (scripts/test-native-cross-service.mjs)")
	}

	require.True(t, strings.HasPrefix(walletURL, "http://127.0.0.1:"))

	ctx := context.Background()

	// The fixture serves the wallet routes; the balance read the relay and
	// the outbox make first is answered here.
	target, err := url.Parse(walletURL)
	require.NoError(t, err)

	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/internal/wallet/balance" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"balance":100}}`))
			return
		}

		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)

	wallet := balance.NewExternalHTTP(front.URL, crossServiceWalletKey)
	oldBalance := balance.Default
	balance.Default = wallet
	t.Cleanup(func() { balance.Default = oldBalance })

	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "gateway-logdb.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.Log{}, &model.AsyncUsageInfo{}, &model.NativeTask{}, &model.ImageTask{},
		&model.RequestDetail{}, &model.ConsumeError{}, &model.Group{}, &model.GroupModelConfig{},
	))
	require.NoError(t, db.Create(&model.Group{ID: crossServiceGroup, Status: model.GroupStatusEnabled}).Error)

	oldLogDB, oldDB := model.LogDB, model.DB
	model.LogDB, model.DB = db, db
	t.Cleanup(func() { model.LogDB, model.DB = oldLogDB, oldDB })

	// One fixed instant, earlier than both the fixture's clock and now, with
	// sub-millisecond digits the wallet must not see.
	at := time.Date(2026, 9, 24, 12, 0, 0, 123_456_789, time.UTC)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	charges := map[string]int64{}

	// 1. Native submission, refused by the provider, settled as not accepted.
	nativeID := "attr-native-" + suffix
	engine := &nativetask.Engine{DB: db, Wallet: wallet, Provider: rejectingNativeProvider{}}
	plan := nativetask.Plan{
		KeyFingerprint: model.ImageChannelKeyFingerprint("test-native-key"),
		Contract:       json.RawMessage(`{"version":1,"model":"brand/model/native","input_schema":{"type":"object","required":["seed"],"properties":{"seed":{"type":"integer"}},"additionalProperties":false},"output_schema":{"type":"object"}}`),
		ChannelID:      1, Endpoint: "fal-ai/vector/model", CredentialScope: "scope",
		QuoteJSON: `{"version":1,"currency":"USD","quoteVersion":"verified","prepaidMicros":200,"routes":[{"routeId":"r","channelId":1,"provider":"fal","endpoint":"fal-ai/vector/model","credentialScope":"scope","quantityMetric":"bounded_request","estimatedMicros":100,"prepaidMicros":200,"rule":{"mode":"cost_markup","ratio":"1"}}]}`,
		Log: &model.NativeTaskLog{
			RequestAt: at, TokenName: model.PlaygroundCredentialName, Endpoint: "POST /v1/model-tasks",
			RequestSource: model.RequestSourcePlayground, Mode: int(mode.NativeTasks),
		},
	}
	nativeTask, err := engine.Submit(ctx, nativeID, crossServiceGroup, 1, []byte(`{"model":"brand/model/native","input":{"seed":1}}`), plan)
	require.NoError(t, err)
	require.Equal(t, "failed", nativeTask.Status)
	require.NoError(t, engine.SyncBilling(ctx, nativeTask))

	var nativeReceipt balance.PrepaymentReceipt
	require.NoError(t, json.Unmarshal([]byte(nativeTask.BillingReceiptJSON), &nativeReceipt))
	require.NotNil(t, nativeReceipt.ChargedMicros)
	require.NotNil(t, nativeReceipt.UsageAtMs, "the application returns the hold's time")
	require.Equal(t, at.UnixMilli(), *nativeReceipt.UsageAtMs)
	charges[nativeTask.BillingOperationID] = *nativeReceipt.ChargedMicros

	// 2. Prepaid image task: the reservation row, then admit, attempt, refund.
	imageID := "attr-image-" + suffix
	imageOp := uuid.NewString()
	imageTask := &model.ImageTask{
		ID: imageID, GroupID: crossServiceGroup, TokenID: 1, Model: "image-entry", Fingerprint: "f",
		BillingOperationID: imageOp, UpstreamModel: "fal-ai/test",
		PrepaymentQuoteJSON: `{"version":1,"quoteVersion":"test-1","currency":"USD","prepaidMicros":2000000,"routes":[{"routeId":"primary","channelId":7,"provider":"fal","endpoint":"fal-ai/test","credentialScope":"channel-7","estimatedMicros":1000000,"prepaidMicros":2000000,"rule":{"mode":"list_ratio","ratio":"1"}}]}`,
	}
	imageFields := model.OperationalFields{RequestSource: model.RequestSourceAPI, PublicModel: "vendor/image", ResolvedCapability: "text-to-image"}
	imageInfo := &model.AsyncUsageInfo{
		RequestID: imageID, BillingOperationID: imageOp, RequestAt: at.Add(2 * time.Second),
		GroupID: crossServiceGroup, TokenID: 1, TokenName: "prod", Model: "image-entry",
		Capability: "text-to-image", Mode: int(mode.ImagesGenerations), ChannelID: 7,
	}
	logModel, logCapability := model.CallLogModel(imageInfo.Model, imageInfo.Capability, imageFields)
	imageInfo.SetUsageIdentity(model.NewUsageIdentity(1, logModel, logCapability, imageFields.RequestSource, at))
	_, created, err := model.ReserveImageTask(imageTask, imageInfo, imageFields)
	require.NoError(t, err)
	require.True(t, created)

	imageUsage, ok := imageInfo.UsageIdentity()
	require.True(t, ok)
	require.NoError(t, imageprepayment.Admit(ctx, imageTask, 7, imageUsage))
	claimed, err := imageprepayment.Begin(ctx, imageTask, 7, "fal-ai/test", 0)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, imageprepayment.Reject(ctx, imageTask, 0))
	imageReceipt, err := imageprepayment.Call(ctx, imageTask, balance.PrepaymentCommand{
		Action: "settle", Outcome: &balance.PrepaymentOutcome{Kind: "failed", Reason: "not_accepted"},
	})
	require.NoError(t, err)
	require.NotNil(t, imageReceipt.ChargedMicros)
	charges[imageOp] = *imageReceipt.ChargedMicros

	// 3. Relay consume (sync call) with the hidden Playground credential.
	_, consumer, err := wallet.GetGroupRemainBalance(ctx, model.GroupCache{ID: crossServiceGroup})
	require.NoError(t, err)

	// The wallet requires the retail pricing provenance on every consume.
	pricing := model.ModelConfig{Config: map[model.ModelConfigKey]any{
		"x_token_platform_pricing": map[string]any{"currency": "USD", "pricing_version": "p2-cross-service"},
	}}

	relayOp := uuid.NewString()
	relayMeta := &meta.Meta{
		ModelConfig: pricing,
		RequestID:   "attr-relay-" + suffix, BillingOperationID: relayOp, RequestAt: at,
		Group: model.GroupCache{ID: crossServiceGroup}, Token: model.TokenCache{ID: 1, Name: model.PlaygroundCredentialName},
		OriginModel: "gpt-upstream", Mode: mode.ChatCompletions,
		OperationalFields: model.OperationalFields{
			RequestSource: model.RequestSourcePlayground, PublicModel: "openai/gpt-5",
			ResolvedCapability: "chat", BillingOperationID: relayOp,
		},
	}
	consume.Consume(ctx, time.Now(), consumer, time.Time{}, http.StatusOK, relayMeta,
		model.Usage{}, model.UsageContext{}, model.Price{PerRequestPrice: 0.01}, "", "127.0.0.1", 0, nil, true, nil, "",
		model.AsyncUsageStatusNone)
	charges[relayOp] = 10_000

	// 4. Video: the submit row and outbox the relay writes, then the outbox
	// consume.
	videoOp := uuid.NewString()
	videoMeta := &meta.Meta{
		ModelConfig: pricing,
		RequestID:   "attr-video-" + suffix, BillingOperationID: videoOp, RequestAt: at,
		Group: model.GroupCache{ID: crossServiceGroup}, Token: model.TokenCache{ID: 1, Name: "prod"},
		OriginModel: "veo-3", VideoCapability: "text-to-video", Mode: mode.Videos,
		OperationalFields: model.OperationalFields{RequestSource: model.RequestSourceAPI, BillingOperationID: videoOp},
	}
	consume.Consume(ctx, time.Now(), consumer, time.Time{}, http.StatusOK, videoMeta,
		model.Usage{}, model.UsageContext{}, model.Price{}, "", "127.0.0.1", 0, nil, true, nil, "upstream-video",
		model.AsyncUsageStatusPending)

	videoInfo := &model.AsyncUsageInfo{
		RequestID: videoMeta.RequestID, BillingOperationID: videoOp, RequestAt: at,
		Mode: int(mode.Videos), Model: "veo-3", Capability: "text-to-video",
		GroupID: crossServiceGroup, TokenID: 1, TokenName: "prod",
		Price: model.Price{PerRequestPrice: 0.02}, UpstreamID: "upstream-video", ProcessingToken: "claim-video",
		PricingCurrency: "USD", PricingVersion: "p2-cross-service",
	}
	videoInfo.SetUsageIdentity(consume.RelayUsageIdentity(videoMeta))
	require.NoError(t, model.CreateAsyncUsageInfo(videoInfo))
	require.NoError(t, completeAsyncUsage(ctx, videoInfo, model.Usage{}, model.UsageContext{}))
	require.True(t, videoInfo.BalanceConsumed)
	charges[videoOp] = 20_000

	// Compare the wallet ledger with the gateway's call rows, per operation.
	raw, err := json.Marshal(map[string]any{})
	require.NoError(t, err)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, walletURL+"/_test/ledger", bytes.NewReader(raw))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+crossServiceWalletKey)

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer response.Body.Close()

	var ledger struct {
		Code int                     `json:"code"`
		Data []crossServiceLedgerRow `json:"data"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&ledger))
	require.Zero(t, ledger.Code)

	byOperation := map[string][]crossServiceLedgerRow{}

	for _, row := range ledger.Data {
		if row.OperationID != nil {
			byOperation[*row.OperationID] = append(byOperation[*row.OperationID], row)
		}
	}

	for op, charge := range charges {
		t.Run(op, func(t *testing.T) {
			var calls []model.Log
			require.NoError(t, db.Where("billing_operation_id = ?", op).Order("request_at, id").Find(&calls).Error)
			require.NotEmpty(t, calls, "the operation has a call row")

			anchor := &calls[0]
			for index := range calls {
				if calls[index].Code == http.StatusAccepted {
					anchor = &calls[index]
					break
				}
			}

			identity := anchor.UsageIdentity()
			rows := byOperation[op]
			require.NotEmpty(t, rows, "the operation moved money")

			total := int64(0)
			for _, row := range rows {
				require.NotNil(t, row.APIKeyID, row.RequestID)
				require.NotNil(t, row.ModelID, row.RequestID)
				require.NotNil(t, row.Source, row.RequestID)
				require.NotNil(t, row.UsageAt, row.RequestID)
				require.EqualValues(t, identity.APIKeyID, *row.APIKeyID, row.RequestID)
				require.Equal(t, identity.ModelID, *row.ModelID, row.RequestID)
				require.Equal(t, identity.Source, *row.Source, row.RequestID)
				require.Equal(t, identity.UsageAtMs(), *row.UsageAt, row.RequestID)

				total += row.Amount
			}

			require.Equal(t, charge, -total, "−Σ amount_micros is the final charge")
		})
	}
}
