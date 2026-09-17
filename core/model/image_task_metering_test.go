package model

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestQueueImageBudgetUsesMaximumTariffAndPerOutputReferences(t *testing.T) {
	max := int64(100)
	p := Price{ImageBilling: &ImageBillingPolicy{Version: 1, Scenario: "generation", Input: &ImageBillingInput{ChargeBasis: "per_output", FirstNFree: 1, AmountMicros: 100000, UnitQuantity: 1}, OutputPixelTiers: []ImageBillingTier{{MaxPixels: &max, AmountMicros: 900000, UnitQuantity: 1}, {AmountMicros: 200000, UnitQuantity: 1}}}}
	got, err := QueueImageMaximumAmount(p, 3, 2)
	require.NoError(t, err)
	require.Equal(t, 3.0, got)
	p.ConditionalPrices = []ConditionalPrice{{}}
	_, err = QueueImageMaximumAmount(p, 3, 2)
	require.Error(t, err)
}
func TestImageOutputDimensionsSurviveJSON(t *testing.T) {
	var out ImageOutput
	require.NoError(t, json.Unmarshal([]byte(`{"url":"https://cdn.example/a","width":512,"height":1024}`), &out))
	require.Equal(t, int64(512), *out.Width)
}
