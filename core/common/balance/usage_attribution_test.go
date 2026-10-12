package balance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

var testUsage = model.UsageIdentity{
	APIKeyID: 7,
	ModelID:  "vendor/model/text-to-image",
	Source:   model.RequestSourcePlayground,
	UsageAt:  time.UnixMilli(1_791_000_000_123).UTC(),
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()

	var body map[string]any
	require.NoError(t, json.NewDecoder(r.Body).Decode(&body))

	return body
}

func requireUsageFields(t *testing.T, body map[string]any) {
	t.Helper()
	require.EqualValues(t, 7, body["apiKeyId"])
	require.Equal(t, "vendor/model/text-to-image", body["usageModelId"])
	require.Equal(t, "playground", body["source"])
	require.EqualValues(t, 1_791_000_000_123, body["usageAtMs"])
}

func requireNoUsageFields(t *testing.T, body map[string]any) {
	t.Helper()

	for _, key := range []string{"apiKeyId", "usageModelId", "source", "usageAtMs"} {
		require.NotContains(t, body, key)
	}
}

// Key usage P2: consume carries the call's attribution next to its op, all
// four fields or none. Old: neither existed.
func TestExternalConsumeCarriesUsageIdentity(t *testing.T) {
	setFastExternalRetries(t)

	var bodies []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodies = append(bodies, decodeBody(t, r))
		writeExternalCharged(w, 0.5)
	}))
	defer srv.Close()

	consumer := newExternalPostGroupConsumer(NewExternalHTTP(srv.URL, "k"), "u_1")

	ctx := ContextWithBillingOperationID(context.Background(), "op-1")
	ctx = ContextWithUsageIdentity(ctx, testUsage)
	_, err := consumer.PostGroupConsume(ctx, PlaygroundTokenNameForTest, 0.5)
	require.NoError(t, err)

	partial := testUsage
	partial.APIKeyID = 0
	_, err = consumer.PostGroupConsume(
		ContextWithUsageIdentity(ContextWithBillingOperationID(context.Background(), "op-2"), partial),
		"prod", 0.5,
	)
	require.NoError(t, err)

	_, err = consumer.PostGroupConsume(ContextWithBillingOperationID(context.Background(), "op-3"), "prod", 0.5)
	require.NoError(t, err)

	require.Len(t, bodies, 3)
	require.Equal(t, "op-1", bodies[0]["billingOperationId"])
	requireUsageFields(t, bodies[0])
	requireNoUsageFields(t, bodies[1])
	requireNoUsageFields(t, bodies[2])
}

const PlaygroundTokenNameForTest = model.PlaygroundCredentialName

func TestPrepaymentReceiptUsageFields(t *testing.T) {
	var commands []map[string]any

	reply := `{"code":0,"data":{"id":"op-1","status":"pending","prepaidMicros":200,"usageAtMs":1791000000123,"usageSource":"playground"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		commands = append(commands, decodeBody(t, r))
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()

	client := NewExternalHTTP(srv.URL, "k")
	prepaid := int64(200)
	admit := PrepaymentCommand{Action: "admit", Group: "u_1", BillingOperationID: "op-1", ModelID: "entry", PrepaidMicros: &prepaid}

	receipt, err := client.Prepayment(context.Background(), admit.WithUsage(testUsage))
	require.NoError(t, err)
	require.NotNil(t, receipt.UsageAtMs)
	require.EqualValues(t, 1_791_000_000_123, *receipt.UsageAtMs)
	require.Equal(t, "playground", receipt.UsageSource)
	requireUsageFields(t, commands[0])
	require.Equal(t, "entry", commands[0]["modelId"], "modelId keeps its meaning")

	// An incomplete identity sends nothing.
	_, err = client.Prepayment(context.Background(), admit.WithUsage(model.UsageIdentity{ModelID: "m"}))
	require.NoError(t, err)
	requireNoUsageFields(t, commands[1])

	for name, body := range map[string]string{
		// An application without key usage P2 returns neither field.
		"old app":        `{"code":0,"data":{"id":"op-1","status":"pending","prepaidMicros":200}}`,
		"null":           `{"code":0,"data":{"id":"op-1","status":"pending","prepaidMicros":200,"usageAtMs":null,"usageSource":null}}`,
		"zero time":      `{"code":0,"data":{"id":"op-1","status":"pending","prepaidMicros":200,"usageAtMs":0,"usageSource":"api"}}`,
		"unknown source": `{"code":0,"data":{"id":"op-1","status":"pending","prepaidMicros":200,"usageAtMs":1791000000123,"usageSource":"web"}}`,
		"time only":      `{"code":0,"data":{"id":"op-1","status":"pending","prepaidMicros":200,"usageAtMs":1791000000123}}`,
	} {
		reply = body
		receipt, err := client.Prepayment(context.Background(), PrepaymentCommand{Action: "get", Group: "u_1", BillingOperationID: "op-1"})
		require.NoError(t, err, name)
		require.Nil(t, receipt.UsageAtMs, name)
		require.Empty(t, receipt.UsageSource, name)
	}
}

func TestMeasuredZeroClient(t *testing.T) {
	var (
		bodies []map[string]any
		reply  = `{"code":0,"data":{"receipt":"created"}}`
		status = http.StatusOK
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/internal/wallet/measured-zero", r.URL.Path)
		require.Equal(t, "Bearer wallet-key", r.Header.Get("Authorization"))
		bodies = append(bodies, decodeBody(t, r))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()

	client := NewExternalHTTP(srv.URL, "wallet-key")
	var _ MeasuredZeroReporter = client

	request := MeasuredZeroRequest{
		Group: "u_1", RequestID: "req-1", BillingOperationID: "op-1",
		PricingVersion: "v3", TokenName: "prod", Usage: testUsage,
	}
	receipt, err := client.MeasuredZero(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, "created", receipt)
	require.Equal(t, "u_1", bodies[0]["group"])
	require.Equal(t, "req-1", bodies[0]["requestId"])
	require.Equal(t, "op-1", bodies[0]["billingOperationId"])
	require.Equal(t, "v3", bodies[0]["pricingVersion"])
	require.Equal(t, "prod", bodies[0]["tokenName"])
	requireUsageFields(t, bodies[0])

	for _, answer := range []string{"existing", "not_applicable"} {
		reply = `{"code":0,"data":{"receipt":"` + answer + `"}}`
		receipt, err = client.MeasuredZero(context.Background(), request)
		require.NoError(t, err)
		require.Equal(t, answer, receipt)
	}

	for name, answer := range map[string]struct {
		status int
		body   string
	}{
		"non-zero code":   {http.StatusOK, `{"code":-1,"message":"failed"}`},
		"missing code":    {http.StatusOK, `{"data":{"receipt":"created"}}`},
		"unknown receipt": {http.StatusOK, `{"code":0,"data":{"receipt":"maybe"}}`},
		"not json":        {http.StatusOK, `oops`},
		"not found":       {http.StatusNotFound, `{}`},
	} {
		status, reply = answer.status, answer.body
		_, err = client.MeasuredZero(context.Background(), request)
		require.Error(t, err, name)
	}

	status, reply = http.StatusOK, `{"code":0,"data":{"receipt":"created"}}`
	request.Usage = model.UsageIdentity{}
	_, err = client.MeasuredZero(context.Background(), request)
	require.NoError(t, err)
	requireNoUsageFields(t, bodies[len(bodies)-1])

	_, err = client.MeasuredZero(context.Background(), MeasuredZeroRequest{Group: "u_1"})
	require.Error(t, err, "a request id is required")
}
