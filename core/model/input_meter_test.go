package model

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const meterQuoteJSON = `{"currency":"USD","prepaidMicros":200000,"quoteVersion":"release:108","routes":[{"channelId":48,"credentialScope":"scope","endpoint":"elevenlabs/tts/eleven-v4-turbo","estimatedMicros":200000,"prepaidMicros":200000,"provider":"fal","quantityMetric":"request","routeId":"turbo","rule":{"mode":"list_ratio","ratio":"1"}}],"settlementPolicy":"actual-cost-v1","version":1}`
const meterJSON = `{"version":1,"metric":"characters","path":["text"],"routes":{"turbo":{"unitSize":1000,"unitMicros":40000,"maxQuantity":5000,"maxMicros":200000}}}`

func TestInputMeterHoldIsTheTextPriceRoundedUpAndCapped(t *testing.T) {
	turbo := InputMeterRoute{UnitSize: 1000, UnitMicros: 40000, MaxQuantity: 5000, MaxMicros: 200000}
	for quantity, want := range map[int64]int64{122: 4880, 1: 40, 0: 40, 4999: 199960, 5000: 200000, 5001: 200000} {
		require.Equal(t, want, turbo.Hold(quantity), "quantity %d", quantity)
	}
	// A unit price that is not a whole number of micros per character rounds up.
	odd := InputMeterRoute{UnitSize: 1000, UnitMicros: 33334, MaxQuantity: 5000, MaxMicros: 166670}
	require.EqualValues(t, 34, odd.Hold(1))  // 33.334
	require.EqualValues(t, 101, odd.Hold(3)) // 100.002
	tiny := InputMeterRoute{UnitSize: 1000, UnitMicros: 1, MaxQuantity: 1000, MaxMicros: 1}
	require.EqualValues(t, 1, tiny.Hold(0), "a hold is never zero")
}

func TestParseInputMeterRequiresAUsableConsistentMeter(t *testing.T) {
	quote, err := ParseImagePrepaymentQuote(meterQuoteJSON)
	require.NoError(t, err)
	meter, err := ParseInputMeter([]byte(meterJSON), quote)
	require.NoError(t, err)
	require.Equal(t, []string{"text"}, meter.Path)
	// Unknown fields are ignored: the meter is decoded leniently.
	_, err = ParseInputMeter([]byte(strings.Replace(meterJSON, `"version":1`, `"version":1,"note":"later field"`, 1)), quote)
	require.NoError(t, err)

	for name, raw := range map[string]string{
		"empty":                "",
		"not json":             `{"version":1`,
		"null":                 `null`,
		"version 2":            strings.Replace(meterJSON, `"version":1`, `"version":2`, 1),
		"other metric":         strings.Replace(meterJSON, `"characters"`, `"utf16_units"`, 1),
		"no path":              strings.Replace(meterJSON, `["text"]`, `[]`, 1),
		"empty path":           strings.Replace(meterJSON, `["text"]`, `[""]`, 1),
		"nested path":          strings.Replace(meterJSON, `["text"]`, `["input","text"]`, 1),
		"no routes":            `{"version":1,"metric":"characters","path":["text"],"routes":{}}`,
		"unknown route":        strings.Replace(meterJSON, `"turbo":`, `"other":`, 1),
		"fractional unit":      strings.Replace(meterJSON, `"unitMicros":40000`, `"unitMicros":40000.5`, 1),
		"zero unit size":       strings.Replace(meterJSON, `"unitSize":1000`, `"unitSize":0`, 1),
		"unit size too large":  strings.Replace(meterJSON, `"unitSize":1000`, `"unitSize":1000001`, 1),
		"zero unit price":      strings.Replace(meterJSON, `"unitMicros":40000`, `"unitMicros":0`, 1),
		"unit price too large": strings.Replace(meterJSON, `"unitMicros":40000`, `"unitMicros":1000000001`, 1),
		"quantity too large":   strings.Replace(meterJSON, `"maxQuantity":5000`, `"maxQuantity":1000001`, 1),
		"cap is not the quote": strings.Replace(meterJSON, `"maxMicros":200000`, `"maxMicros":199999`, 1),
		// 5000 characters cost 200,000; a cap one unit price higher is not this meter's maximum.
		"cap above the longest text": `{"version":1,"metric":"characters","path":["text"],"routes":{"turbo":{"unitSize":1000,"unitMicros":40000,"maxQuantity":4000,"maxMicros":200000}}}`,
	} {
		_, err := ParseInputMeter([]byte(raw), quote)
		require.Error(t, err, name)
	}

	withoutPolicy, err := ParseImagePrepaymentQuote(strings.Replace(meterQuoteJSON, `,"settlementPolicy":"actual-cost-v1"`, ``, 1))
	require.NoError(t, err)
	_, err = ParseInputMeter([]byte(meterJSON), withoutPolicy)
	require.ErrorContains(t, err, "actual-cost")
	perImage, err := ParseImagePrepaymentQuote(strings.Replace(meterQuoteJSON, `"quantityMetric":"request"`, `"quantityMetric":"image"`, 1))
	require.NoError(t, err)
	_, err = ParseInputMeter([]byte(meterJSON), perImage)
	require.Error(t, err)
	// Within one unit price below the cap is the ceil of a whole number of units.
	rounded, err := ParseImagePrepaymentQuote(strings.ReplaceAll(meterQuoteJSON, "200000", "120000"))
	require.NoError(t, err)
	_, err = ParseInputMeter([]byte(`{"version":1,"metric":"characters","path":["text"],"routes":{"turbo":{"unitSize":1000,"unitMicros":40000,"maxQuantity":2500,"maxMicros":120000}}}`), rounded)
	require.NoError(t, err)
}

// A unit price that is not a whole number of micro-USD is published rounded
// up, so the longest text can price a few micros above the cap. The meter stays
// usable (the hold is capped), as the application decides when it publishes
// the meter: both repositories assert these cases.
func TestParseInputMeterAcceptsAUnitPriceRoundedUp(t *testing.T) {
	for name, tc := range map[string]struct {
		route    InputMeterRoute
		uncapped int64
		holds    map[int64]int64
	}{
		// $0.04 per 1,000 characters at sales ratio 1.33333: 53,333.2 µ$.
		"ratio 1.33333": {InputMeterRoute{UnitSize: 1000, UnitMicros: 53334, MaxQuantity: 5000, MaxMicros: 266666}, 266670,
			map[int64]int64{100: 5334, 122: 6507, 4999: 266617, 5000: 266666}},
		// $0.0225 per 1,000 characters at sales ratio 1.125: 25,312.5 µ$.
		"ratio 1.125": {InputMeterRoute{UnitSize: 1000, UnitMicros: 25313, MaxQuantity: 5000, MaxMicros: 126563}, 126565,
			map[int64]int64{100: 2532, 4999: 126540, 5000: 126563}},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.uncapped, tc.route.price(tc.route.MaxQuantity), "the rounded unit price overshoots the cap")
			raw := strings.ReplaceAll(meterQuoteJSON, "200000", strconv.FormatInt(tc.route.MaxMicros, 10))
			quote, err := ParseImagePrepaymentQuote(raw)
			require.NoError(t, err)
			encoded, err := json.Marshal(InputMeter{Version: 1, Metric: "characters", Path: []string{"text"}, Routes: map[string]InputMeterRoute{"turbo": tc.route}})
			require.NoError(t, err)
			meter, err := ParseInputMeter(encoded, quote)
			require.NoError(t, err)
			for quantity, hold := range tc.holds {
				require.Equal(t, hold, meter.Routes["turbo"].Hold(quantity), "quantity %d", quantity)
			}
		})
	}
	// The cap must still be the hold of the longest text within one unit
	// price: a unit price far below it is refused.
	quote, err := ParseImagePrepaymentQuote(meterQuoteJSON)
	require.NoError(t, err)
	_, err = ParseInputMeter([]byte(`{"version":1,"metric":"characters","path":["text"],"routes":{"turbo":{"unitSize":1000,"unitMicros":10000,"maxQuantity":5000,"maxMicros":200000}}}`), quote)
	require.Error(t, err, "a unit price far below the cap's would hold too little")
}

func TestInputMeterApplyRewritesOnlyAmounts(t *testing.T) {
	quote, err := ParseImagePrepaymentQuote(meterQuoteJSON)
	require.NoError(t, err)
	meter, err := ParseInputMeter([]byte(meterJSON), quote)
	require.NoError(t, err)
	sized, encoded, err := meter.Apply(meterQuoteJSON, quote, 122)
	require.NoError(t, err)
	require.Equal(t, strings.NewReplacer(`"prepaidMicros":200000`, `"prepaidMicros":4880`, `"estimatedMicros":200000`, `"estimatedMicros":4880`).Replace(meterQuoteJSON), encoded)
	require.EqualValues(t, 4880, sized.PrepaidMicros)
	require.EqualValues(t, 200000, quote.PrepaidMicros, "the published quote is not modified")
	again, encodedAgain, err := meter.Apply(meterQuoteJSON, quote, 122)
	require.NoError(t, err)
	require.Equal(t, encoded, encodedAgain)
	require.Equal(t, sized, again)

	// The longest text holds the published maximum: the quote stays byte for byte.
	_, unchanged, err := meter.Apply(meterQuoteJSON, quote, 5000)
	require.NoError(t, err)
	require.Equal(t, meterQuoteJSON, unchanged)
	_, _, err = meter.Apply(meterQuoteJSON, quote, 5001)
	require.Error(t, err)
}

// A quote sent by a private trial may use any key order. Only the amounts of
// the metered route and the top-level maximum change; the unmetered route and
// every other value keep their text, and keys come out sorted.
func TestInputMeterApplyKeepsUnmeteredRoutesAndValues(t *testing.T) {
	raw := `{ "version": 1, "currency": "USD", "quoteVersion": "a&b<c>", "settlementPolicy": "actual-cost-v1", "prepaidMicros": 200000,
	  "routes": [
	    {"routeId": "metered", "channelId": 1, "provider": "fal", "endpoint": "e/one", "credentialScope": "s", "quantityMetric": "request", "estimatedMicros": 150000, "prepaidMicros": 200000, "rule": {"mode": "cost_markup", "ratio": "1.150"}},
	    {"routeId": "fixed", "channelId": 2, "provider": "fal", "endpoint": "e/two", "credentialScope": "s", "quantityMetric": "request", "estimatedMicros": 30, "prepaidMicros": 60, "rule": {"mode": "list_ratio", "ratio": "1"}}
	  ]}`
	quote, err := ParseImagePrepaymentQuote(raw)
	require.NoError(t, err)
	meter, err := ParseInputMeter([]byte(`{"version":1,"metric":"characters","path":["text"],"routes":{"metered":{"unitSize":1000,"unitMicros":40000,"maxQuantity":5000,"maxMicros":200000}}}`), quote)
	require.NoError(t, err)
	sized, encoded, err := meter.Apply(raw, quote, 1)
	require.NoError(t, err)
	require.Equal(t, `{"currency":"USD","prepaidMicros":60,"quoteVersion":"a&b<c>","routes":[{"channelId":1,"credentialScope":"s","endpoint":"e/one","estimatedMicros":40,"prepaidMicros":40,"provider":"fal","quantityMetric":"request","routeId":"metered","rule":{"mode":"cost_markup","ratio":"1.150"}},{"channelId":2,"credentialScope":"s","endpoint":"e/two","estimatedMicros":30,"prepaidMicros":60,"provider":"fal","quantityMetric":"request","routeId":"fixed","rule":{"mode":"list_ratio","ratio":"1"}}],"settlementPolicy":"actual-cost-v1","version":1}`, encoded)
	require.EqualValues(t, 60, sized.PrepaidMicros, "the top level covers every route")
	require.EqualValues(t, 30, sized.Routes[1].EstimatedMicros)
}

func TestModelConfigInputMeterJSON(t *testing.T) {
	var value any
	require.NoError(t, json.Unmarshal([]byte(meterJSON), &value))
	config := ModelConfig{Config: map[ModelConfigKey]any{InputMeterConfigKey: value}}
	quote, err := ParseImagePrepaymentQuote(meterQuoteJSON)
	require.NoError(t, err)
	_, err = ParseInputMeter(config.InputMeterJSON(), quote)
	require.NoError(t, err, "a meter read back from model config stays usable")
	require.Nil(t, (&ModelConfig{}).InputMeterJSON())
	require.Equal(t, ModelConfigKey("x_token_platform_input_meter_v1"), InputMeterConfigKey)
}
