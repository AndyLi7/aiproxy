//nolint:testpackage // These fixtures verify internal metering and persistence boundaries.
package consume

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestMeasuredImageAmountDoesNotUseLegacyFallback(t *testing.T) {
	var price model.Price
	require.NoError(
		t,
		json.Unmarshal(
			[]byte(
				`{"output_price":9,"output_price_unit":1,"image_billing":{"version":1,"scenario":"generation","outputPixelTiers":[{"maxPixels":null,"amountMicros":300000,"unitQuantity":1}]}}`,
			),
			&price,
		),
	)

	for _, tc := range []struct {
		state string
		code  int
		want  float64
	}{{"complete", http.StatusOK, .3}, {"incomplete", http.StatusOK, 0}, {"failed", http.StatusOK, 0}, {"complete", http.StatusBadGateway, 0}} {
		w := int64(100)
		u := &model.ImageUsage{
			Version:  1,
			State:    tc.state,
			Scenario: "generation",
			Outputs:  []model.ImageUsageOutput{{Index: 0, Width: &w, Height: &w}},
		}
		got := CalculateAmountDetail(
			tc.code,
			model.Usage{OutputTokens: 99},
			model.UsageContext{ImageUsage: u},
			price,
		)
		require.Equal(t, tc.want, got.UsedAmount)
	}
}
