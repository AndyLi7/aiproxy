package consume_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/consume"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Key usage P2: a relay call's consume carries the meta op and the identity of
// the call row it writes, and the row carries the same op. Old: the body had
// no attribution and the row had no op column.
func TestRelayConsumeCarriesUsageIdentity(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}))

	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = oldLogDB })

	var consumed map[string]any

	wallet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/internal/wallet/balance":
			_, _ = w.Write([]byte(`{"code":0,"data":{"balance":100}}`))
		case "/api/internal/wallet/consume":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&consumed))

			_, _ = w.Write([]byte(`{"code":0,"data":{"charged":0.002}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(wallet.Close)

	external := balance.NewExternalHTTP(wallet.URL, "k")
	group := model.GroupCache{ID: "u_relay_identity"}
	_, consumer, err := external.GetGroupRemainBalance(context.Background(), group)
	require.NoError(t, err)

	requestAt := time.Date(2026, time.October, 12, 1, 2, 3, 456_789_000, time.UTC)
	requestMeta := &meta.Meta{
		RequestID:          "relay-identity",
		BillingOperationID: "op-relay",
		RequestAt:          requestAt,
		Group:              group,
		Token:              model.TokenCache{ID: 9, Name: model.PlaygroundCredentialName},
		OriginModel:        "gpt-upstream",
		Mode:               mode.ChatCompletions,
		OperationalFields: model.OperationalFields{
			RequestSource:      model.RequestSourcePlayground,
			PublicModel:        "openai/gpt-5",
			ResolvedCapability: "chat",
			BillingOperationID: "op-relay",
		},
	}

	consume.Consume(
		context.Background(), time.Now(), consumer, time.Time{}, http.StatusOK, requestMeta,
		model.Usage{InputTokens: 2, TotalTokens: 2}, model.UsageContext{},
		model.Price{InputPrice: 1, InputPriceUnit: 1000}, "", "127.0.0.1", 0, nil, true, nil, "",
		model.AsyncUsageStatusNone,
	)

	require.NotNil(t, consumed, "the consume was posted")
	require.Equal(t, "op-relay", consumed["billingOperationId"])
	require.EqualValues(t, 9, consumed["apiKeyId"])
	require.Equal(t, "openai/gpt-5/chat", consumed["usageModelId"])
	require.Equal(t, model.RequestSourcePlayground, consumed["source"])
	require.EqualValues(t, requestAt.UnixMilli(), consumed["usageAtMs"])

	var row model.Log
	require.NoError(t, db.Where("request_id = ?", "relay-identity").First(&row).Error)
	require.Equal(t, model.EmptyNullString("op-relay"), row.BillingOperationID)

	identity := row.UsageIdentity()
	require.Equal(t, consume.RelayUsageIdentity(requestMeta), identity)
	require.EqualValues(t, consumed["usageAtMs"], identity.UsageAtMs())
	require.Equal(t, consumed["usageModelId"], identity.ModelID)
}
