package model

import (
	"encoding/json"
	"errors"
)

const ImagePrepaymentConfigKey ModelConfigKey = "upstream_prepayment_v1"
const maximumSafeMicros int64 = 9007199254740991

type ImagePrepaymentRoute struct {
	RouteID         string `json:"routeId"`
	ChannelID       int    `json:"channelId"`
	Provider        string `json:"provider"`
	Endpoint        string `json:"endpoint"`
	CredentialScope string `json:"credentialScope"`
	QuantityMetric  string `json:"quantityMetric,omitempty"`
	EstimatedMicros int64  `json:"estimatedMicros"`
	PrepaidMicros   int64  `json:"prepaidMicros"`
	Rule            struct {
		Mode  string `json:"mode"`
		Ratio string `json:"ratio"`
	} `json:"rule"`
}
type ImagePrepaymentQuote struct {
	SettlementPolicy string                 `json:"settlementPolicy,omitempty"`
	Version          int                    `json:"version"`
	Currency         string                 `json:"currency"`
	QuoteVersion     string                 `json:"quoteVersion"`
	PrepaidMicros    int64                  `json:"prepaidMicros"`
	Routes           []ImagePrepaymentRoute `json:"routes"`
}

func ParseImagePrepaymentQuote(encoded string) (*ImagePrepaymentQuote, error) {
	var quote ImagePrepaymentQuote
	if len(encoded) > 64000 || json.Unmarshal([]byte(encoded), &quote) != nil || quote.Version != 1 || quote.Currency != "USD" || quote.QuoteVersion == "" || len(quote.Routes) < 1 || len(quote.Routes) > 2 {
		return nil, errors.New("invalid upstream prepayment quote")
	}
	if quote.SettlementPolicy != "" && quote.SettlementPolicy != "actual-cost-v1" {
		return nil, errors.New("unsupported prepayment settlement policy")
	}
	max := int64(0)
	for i, route := range quote.Routes {
		if route.QuantityMetric != "" && route.QuantityMetric != "image" && route.QuantityMetric != "request" && route.QuantityMetric != "bounded_request" {
			return nil, errors.New("unsupported prepayment quantity metric")
		}
		if route.RouteID == "" || route.ChannelID <= 0 || route.Provider == "" || route.Endpoint == "" || route.CredentialScope == "" || route.EstimatedMicros < 0 || route.PrepaidMicros < route.EstimatedMicros || route.PrepaidMicros > maximumSafeMicros || (route.Rule.Mode != "list_ratio" && route.Rule.Mode != "cost_markup") || route.Rule.Ratio == "" {
			return nil, errors.New("invalid upstream prepayment route")
		}
		for j := 0; j < i; j++ {
			if quote.Routes[j].RouteID == route.RouteID || quote.Routes[j].ChannelID == route.ChannelID {
				return nil, errors.New("duplicate upstream prepayment route")
			}
		}
		if route.PrepaidMicros > max {
			max = route.PrepaidMicros
		}
	}
	if quote.PrepaidMicros != max {
		return nil, errors.New("prepayment must cover both routes")
	}
	return &quote, nil
}
func (q *ImagePrepaymentQuote) Route(channelID int, endpoint string) *ImagePrepaymentRoute {
	for i := range q.Routes {
		if q.Routes[i].ChannelID == channelID && q.Routes[i].Endpoint == endpoint {
			return &q.Routes[i]
		}
	}
	return nil
}
func (c *ModelConfig) ImagePrepaymentQuote() (*ImagePrepaymentQuote, string, error) {
	raw, exists := c.Config[ImagePrepaymentConfigKey]
	if !exists {
		return nil, "", nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, "", err
	}
	quote, err := ParseImagePrepaymentQuote(string(encoded))
	return quote, string(encoded), err
}

// ForMaximumOutputs freezes the full admission quote using the already validated
// output bound. Request-based prices are never multiplied by the image count.
func (q *ImagePrepaymentQuote) ForMaximumOutputs(count int64) (*ImagePrepaymentQuote, string, error) {
	if count < 1 {
		return nil, "", errors.New("unbounded prepayment output quantity")
	}
	result := *q
	result.Routes = append([]ImagePrepaymentRoute(nil), q.Routes...)
	result.PrepaidMicros = 0
	for i := range result.Routes {
		route := &result.Routes[i]
		if route.QuantityMetric == "image" {
			if route.PrepaidMicros > maximumSafeMicros/count {
				return nil, "", errors.New("prepayment quantity overflows wallet range")
			}
			route.EstimatedMicros *= count
			route.PrepaidMicros *= count
		}
		// Persisted quotes are request totals; recovery must not multiply them again.
		route.QuantityMetric = "bounded_request"
		if route.PrepaidMicros > result.PrepaidMicros {
			result.PrepaidMicros = route.PrepaidMicros
		}
	}
	encoded, err := json.Marshal(result)
	return &result, string(encoded), err
}
