//nolint:testpackage // These fixtures verify internal metering and persistence boundaries.
package model

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

func TestMeasuredImageConfigValidation(t *testing.T) {
	policy := &ImageBillingPolicy{Version: 1, Scenario: "generation"}
	for _, tc := range []struct {
		name    string
		price   Price
		invalid bool
	}{
		{"valid", Price{ImageBilling: policy}, false},
		{"submicro fallback", Price{ImageBilling: policy, OutputPrice: 0.0000001}, true},
		{"unsafe unit", Price{ImageBilling: policy, OutputPriceUnit: ZeroNullInt64(ImageBillingMaxSafeInteger + 1)}, true},
		{"unknown version", Price{ImageBilling: &ImageBillingPolicy{Version: 2, Scenario: "generation"}}, true},
		{"per request mixing", Price{ImageBilling: policy, PerRequestPrice: 1}, true},
		{"base measured legacy override", Price{ImageBilling: policy, ConditionalPrices: []ConditionalPrice{{Condition: PriceCondition{Quality: []string{"hd"}}, Price: Price{OutputPrice: 1}}}}, true},
		{"legacy default measured override", Price{OutputPrice: 1, ConditionalPrices: []ConditionalPrice{{Condition: PriceCondition{Quality: []string{"hd"}}, Price: Price{ImageBilling: policy}}}}, true},
		{"mixed different specificity overlap", Price{ConditionalPrices: []ConditionalPrice{{Price: Price{OutputPrice: 1}}, {Condition: PriceCondition{Quality: []string{"hd"}}, Price: Price{ImageBilling: policy}}}}, true},
		{"disjoint legacy measured", Price{ConditionalPrices: []ConditionalPrice{{Condition: PriceCondition{Quality: []string{"low"}}, Price: Price{OutputPrice: 1}}, {Condition: PriceCondition{Quality: []string{"hd"}}, Price: Price{ImageBilling: policy}}}}, false},
		{"nested", Price{ConditionalPrices: []ConditionalPrice{{Price: Price{ConditionalPrices: []ConditionalPrice{{Price: Price{ImageBilling: policy}}}}}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.price.ValidateConditionalPrices()
			if tc.invalid {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}

	require.Error(
		t,
		(&Price{ConditionalPrices: make([]ConditionalPrice, ImageBillingMaxRules+1)}).ValidateConditionalPrices(),
	)
	require.Error(
		t,
		(&ModelConfig{Model: "text", Type: mode.ChatCompletions, Price: Price{ImageBilling: policy}}).BeforeSave(
			nil,
		),
	)
	require.NoError(
		t,
		(&ModelConfig{Model: "image", Type: mode.ImagesGenerations, Price: Price{ImageBilling: policy}}).BeforeSave(
			nil,
		),
	)

	b, err := json.Marshal(Price{OutputPrice: 1})
	require.NoError(t, err)
	require.NotContains(t, string(b), "image_billing")
}

func TestMeasuredGroupOverridesRequireImageModelAndPreventTypeChange(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "group-image.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&ModelConfig{}, &GroupModelConfig{}))

	policy := &ImageBillingPolicy{Version: 1, Scenario: "generation"}
	for _, tc := range []struct {
		name  string
		kind  mode.Mode
		valid bool
	}{{"chat", mode.ChatCompletions, false}, {"image", mode.ImagesGenerations, true}} {
		mc := ModelConfig{Model: tc.name, Type: tc.kind}
		require.NoError(t, db.Create(&mc).Error)

		override := GroupModelConfig{
			GroupID:       "g",
			Model:         tc.name,
			OverridePrice: true,
			Price:         Price{ImageBilling: policy},
		}

		err = db.Create(&override).Error
		if tc.valid {
			require.NoError(t, err)

			mc.Type = mode.ChatCompletions
			require.Error(t, db.Save(&mc).Error)
		} else {
			require.Error(t, err)
		}
	}
}

func TestMeasuredEffectiveGroupModeValidation(t *testing.T) {
	override := GroupModelConfig{
		OverridePrice: true,
		Price:         Price{ImageBilling: &ImageBillingPolicy{Version: 1, Scenario: "generation"}},
	}
	for _, kind := range []mode.Mode{mode.ChatCompletions, mode.VideoGenerationsJobs, mode.ImagesGenerations, mode.ImagesEdits} {
		base := ModelConfig{Type: kind}

		effective := base.LoadFromGroupModelConfig(override)
		if kind == mode.ImagesGenerations || kind == mode.ImagesEdits {
			require.NoError(t, effective.ValidateImageBillingMode())
		} else {
			require.Error(t, effective.ValidateImageBillingMode())
		}
	}
}
