package model

import (
	"encoding/json"
	"testing"
)

func TestStylePriceSelectionAndPersistence(t *testing.T) {
	var price Price
	if err := json.Unmarshal([]byte(`{"image_output_price":0.04,"image_output_price_unit":1,"conditional_prices":[{"condition":{"style":["vector_illustration","vector_illustration/line_art"]},"price":{"image_output_price":0.08,"image_output_price_unit":1}}]}`), &price); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		style    string
		expected float64
	}{{"realistic_image", 0.04}, {"vector_illustration", 0.08}, {"vector_illustration/line_art", 0.08}} {
		context := UsageContext{Style: row.style}
		saved, err := json.Marshal(context)
		if err != nil {
			t.Fatal(err)
		}
		var restored UsageContext
		if err = json.Unmarshal(saved, &restored); err != nil {
			t.Fatal(err)
		}
		got := price.SelectConditionalPrice(Usage{}, restored)
		if float64(got.ImageOutputPrice) != row.expected {
			t.Fatalf("style %s: got %v", row.style, got.ImageOutputPrice)
		}
	}
	if !styleConditionsOverlap([]string{"a"}, []string{"a"}) || styleConditionsOverlap([]string{"a"}, []string{"b"}) {
		t.Fatal("style overlap incorrect")
	}
	if (UsageContext{}).WithFallback(UsageContext{Style: "a"}).Style != "a" {
		t.Fatal("async context fallback lost style")
	}
	if (UsageContext{Style: "a"}).PriceConditionMatches(PriceCondition{Style: []string{"A"}}) {
		t.Fatal("style must match exact enum")
	}
}
