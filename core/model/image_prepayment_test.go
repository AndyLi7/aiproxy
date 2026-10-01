package model

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPrepaymentQuantityFreezesMaximumAcrossWholeRoutes(t *testing.T) {
	q := ImagePrepaymentQuote{Version: 1, Currency: "USD", QuoteVersion: "v1", PrepaidMicros: 200, Routes: []ImagePrepaymentRoute{
		{RouteID: "image", ChannelID: 1, Provider: "fal", Endpoint: "one", CredentialScope: "a", QuantityMetric: "image", EstimatedMicros: 60, PrepaidMicros: 100},
		{RouteID: "request", ChannelID: 2, Provider: "fal", Endpoint: "two", CredentialScope: "b", QuantityMetric: "request", EstimatedMicros: 180, PrepaidMicros: 200},
	}}
	for i := range q.Routes {
		q.Routes[i].Rule.Mode = "list_ratio"
		q.Routes[i].Rule.Ratio = "1"
	}
	full, encoded, err := q.ForMaximumOutputs(3)
	require.NoError(t, err)
	require.EqualValues(t, 300, full.PrepaidMicros)
	require.EqualValues(t, 180, full.Routes[0].EstimatedMicros)
	require.EqualValues(t, 200, full.Routes[1].PrepaidMicros)
	require.EqualValues(t, 100, q.Routes[0].PrepaidMicros)
	parsed, err := ParseImagePrepaymentQuote(encoded)
	require.NoError(t, err)
	again, _, err := parsed.ForMaximumOutputs(3)
	require.NoError(t, err)
	require.Equal(t, full, again)
	_, _, err = q.ForMaximumOutputs(0)
	require.Error(t, err)
	q.Routes[0].PrepaidMicros = maximumSafeMicros
	_, _, err = q.ForMaximumOutputs(2)
	require.Error(t, err)
}
func TestPrepaymentRejectsUnknownQuantityMeaning(t *testing.T) {
	q := ImagePrepaymentQuote{Version: 1, Currency: "USD", QuoteVersion: "v1", Routes: []ImagePrepaymentRoute{{QuantityMetric: "pixels"}}}
	raw, err := json.Marshal(q)
	require.NoError(t, err)
	_, err = ParseImagePrepaymentQuote(string(raw))
	require.ErrorContains(t, err, "quantity metric")
}

func TestActualCostPolicySurvivesQuantityProjection(t *testing.T) {
	raw := `{"version":1,"currency":"USD","quoteVersion":"new","settlementPolicy":"actual-cost-v1","prepaidMicros":20,"routes":[{"routeId":"r","channelId":1,"provider":"fal","endpoint":"e","credentialScope":"c","quantityMetric":"image","estimatedMicros":10,"prepaidMicros":20,"rule":{"mode":"list_ratio","ratio":"1"}}]}`
	q, err := ParseImagePrepaymentQuote(raw)
	if err != nil {
		t.Fatal(err)
	}
	scaled, encoded, err := q.ForMaximumOutputs(3)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseImagePrepaymentQuote(encoded)
	if err != nil || scaled.PrepaidMicros != 60 || again.SettlementPolicy != "actual-cost-v1" {
		t.Fatalf("lost settlement policy: %s %v", encoded, err)
	}
	q.SettlementPolicy = "unknown"
	unknown, _ := json.Marshal(q)
	if _, err := ParseImagePrepaymentQuote(string(unknown)); err == nil {
		t.Fatal("accepted unknown policy")
	}
}
