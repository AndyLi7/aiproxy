//nolint:testpackage
package model

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQueueImageBudgetUsesMaximumTariffAndPerOutputReferences(t *testing.T) {
	maxPixels := int64(100)
	p := Price{
		ImageBilling: &ImageBillingPolicy{
			Version:  1,
			Scenario: "generation",
			Input: &ImageBillingInput{
				ChargeBasis:  "per_output",
				FirstNFree:   1,
				AmountMicros: 100000,
				UnitQuantity: 1,
			},
			OutputPixelTiers: []ImageBillingTier{
				{MaxPixels: &maxPixels, AmountMicros: 900000, UnitQuantity: 1},
				{AmountMicros: 200000, UnitQuantity: 1},
			},
		},
	}
	got, err := QueueImageMaximumAmount(p, 3, 2)
	require.NoError(t, err)
	require.Equal(t, 3.0, got)

	p.ConditionalPrices = []ConditionalPrice{{}}
	_, err = QueueImageMaximumAmount(p, 3, 2)
	require.Error(t, err)
}

func TestImageOutputDimensionsSurviveJSON(t *testing.T) {
	var out ImageOutput
	require.NoError(
		t,
		json.Unmarshal([]byte(`{"url":"https://cdn.example/a","width":512,"height":1024}`), &out),
	)
	require.Equal(t, int64(512), *out.Width)
}

func TestQueuePixelBudgetRequiresAuditedBound(t *testing.T) {
	p := Price{OutputPrice: 0.003, OutputPriceUnit: 1, ImageBilling: &ImageBillingPolicy{Version: 2, Scenario: "generation", OutputPixels: &ImagePixelQuantity{PixelsPerUnit: 1048576, Rounding: "ceil", Scope: "per_image", MinimumUnits: 1}}}
	_, err := QueueImageMaximumAmount(p, 2, 0)
	require.Error(t, err)
	_, err = QueueImageMaximumAmount(p, 2, 0, 0)
	require.Error(t, err)
	bound, err := QueueImageMaximumAmount(p, 2, 0, 4194304)
	require.NoError(t, err)
	require.Equal(t, 0.024, bound)
	bound, err = QueueImageMaximumAmount(p, 1, 0, 1056*1024)
	require.NoError(t, err)
	require.Equal(t, 0.006, bound)
}
