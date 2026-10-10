package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
)

// InputMeterConfigKey holds a native model's input meter (input-sized hold,
// spec 2026-10-10). It is published next to upstream_prepayment_v1, never
// inside it: the frozen quote keeps its exact structure, so every wallet
// version accepts the quote the gateway sends.
const InputMeterConfigKey ModelConfigKey = "x_token_platform_input_meter_v1"

// InputMeterFeature is advertised in /api/status while input meters apply.
const InputMeterFeature = "native_input_meter_v1"

const (
	InputMeterVersion          = 1
	InputMeterMetricCharacters = "characters"
	maxInputMeterUnitSize      = 1_000_000
	maxInputMeterUnitMicros    = 1_000_000_000
	maxInputMeterQuantity      = 1_000_000
)

// InputMeterRoute prices one quote route per unit of the metered input:
// unitMicros per unitSize units, never more than maxMicros, which is that
// route's published prepaidMicros (the price of maxQuantity units).
type InputMeterRoute struct {
	UnitSize    int64 `json:"unitSize"`
	UnitMicros  int64 `json:"unitMicros"`
	MaxQuantity int64 `json:"maxQuantity"`
	MaxMicros   int64 `json:"maxMicros"`
}

// InputMeter is the value of InputMeterConfigKey. Path names the metered
// top-level input field; Routes is keyed by the quote's routeId.
type InputMeter struct {
	Version int                        `json:"version"`
	Metric  string                     `json:"metric"`
	Path    []string                   `json:"path"`
	Routes  map[string]InputMeterRoute `json:"routes"`
}

// InputMeterJSON returns the raw meter of this model, or nil. Decoding and
// validation happen per request: a meter that cannot be used only means the
// request holds the published maximum.
func (c *ModelConfig) InputMeterJSON() json.RawMessage {
	raw, exists := c.Config[InputMeterConfigKey]
	if !exists || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	return encoded
}

// ParseInputMeter decodes a meter leniently (unknown fields are ignored) and
// returns it only when every metered route is usable with this quote. The
// caller keeps the published maximum on any error.
func ParseInputMeter(raw []byte, quote *ImagePrepaymentQuote) (*InputMeter, error) {
	if quote == nil || quote.SettlementPolicy != "actual-cost-v1" {
		return nil, errors.New("input meter requires actual-cost settlement")
	}
	var meter InputMeter
	if len(raw) == 0 || len(raw) > 64000 || json.Unmarshal(raw, &meter) != nil {
		return nil, errors.New("input meter is not valid JSON")
	}
	if meter.Version != InputMeterVersion || meter.Metric != InputMeterMetricCharacters {
		return nil, errors.New("unsupported input meter version or metric")
	}
	if len(meter.Path) != 1 || meter.Path[0] == "" {
		return nil, errors.New("input meter path must name one top-level field")
	}
	if len(meter.Routes) == 0 {
		return nil, errors.New("input meter has no routes")
	}
	ids := make([]string, 0, len(meter.Routes))
	for id := range meter.Routes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		route := quote.routeByID(id)
		if route == nil {
			return nil, errors.New("input meter names a route outside the quote")
		}
		if route.QuantityMetric != "request" && route.QuantityMetric != "bounded_request" {
			return nil, errors.New("input meter route is not priced per request")
		}
		if !meter.Routes[id].usable(route.PrepaidMicros) {
			return nil, errors.New("input meter route is out of range or inconsistent with its maximum")
		}
	}
	return &meter, nil
}

func (q *ImagePrepaymentQuote) routeByID(id string) *ImagePrepaymentRoute {
	for i := range q.Routes {
		if q.Routes[i].RouteID == id {
			return &q.Routes[i]
		}
	}
	return nil
}

// usable requires bounded, int64-safe parameters whose cap is this route's
// published prepayment and whose hold of maxQuantity units is that cap within
// one unit price: maxMicros − unitMicros < Hold(maxQuantity) ≤ maxMicros (spec
// 2026-10-10 A2, the rule the application's inputMeterRouteConsistent applies).
// The hold is capped: a unit price rounded up to a whole micro-USD can price
// maxQuantity units a few micros above the cap (for example $0.0225 × 1.125
// per 1,000 characters is 25,312.5 µ$, published as 25,313), and the cap,
// which is today's fixed hold, absorbs that.
func (r InputMeterRoute) usable(prepaid int64) bool {
	if r.UnitSize < 1 || r.UnitSize > maxInputMeterUnitSize ||
		r.UnitMicros < 1 || r.UnitMicros > maxInputMeterUnitMicros ||
		r.MaxQuantity < 1 || r.MaxQuantity > maxInputMeterQuantity ||
		r.MaxMicros < 1 || r.MaxMicros != prepaid || r.MaxMicros > maximumSafeMicros ||
		r.MaxQuantity > (maximumSafeMicros-r.UnitSize)/r.UnitMicros {
		return false
	}
	full := r.Hold(r.MaxQuantity)
	return r.MaxMicros-r.UnitMicros < full && full <= r.MaxMicros
}

// price is ceil(quantity × unitMicros / unitSize) for a usable route.
func (r InputMeterRoute) price(quantity int64) int64 {
	return (quantity*r.UnitMicros + r.UnitSize - 1) / r.UnitSize
}

// Hold is the price of max(quantity, 1) units rounded up to one micro-USD and
// capped at maxMicros. There is no margin (owner decision 2026-10-10): a rare
// higher bill is debited from the remaining balance at settlement.
func (r InputMeterRoute) Hold(quantity int64) int64 {
	if r.UnitSize < 1 || r.UnitMicros < 1 || quantity > r.MaxQuantity {
		return r.MaxMicros
	}
	return min(r.MaxMicros, r.price(max(quantity, 1)))
}

// Apply sizes every metered route of the quote to quantity units of the
// metered input. Only estimatedMicros/prepaidMicros of metered routes and the
// top-level prepaidMicros change; every other key and value is kept, so the
// same quote and quantity always yield the same bytes. A quantity above a
// route's maxQuantity is an error: the caller keeps the published maximum.
func (m *InputMeter) Apply(encoded string, quote *ImagePrepaymentQuote, quantity int64) (*ImagePrepaymentQuote, string, error) {
	if m == nil || quote == nil || quantity < 0 {
		return nil, "", errors.New("input meter unavailable")
	}
	holds := make(map[string]int64, len(m.Routes))
	for id, route := range m.Routes {
		if quantity > route.MaxQuantity {
			return nil, "", errors.New("metered input exceeds the route maximum")
		}
		holds[id] = route.Hold(quantity)
	}
	return quote.withRouteHolds(encoded, holds)
}

// withRouteHolds rewrites the amounts inside the encoded quote. Keys are
// written in sorted order (the order json.Marshal gives a published model
// config) and all other values keep their original JSON text. An unchanged
// quote is returned byte for byte.
func (q *ImagePrepaymentQuote) withRouteHolds(encoded string, holds map[string]int64) (*ImagePrepaymentQuote, string, error) {
	invalid := errors.New("input meter cannot rewrite this quote")
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(encoded), &top) != nil || top == nil {
		return nil, "", invalid
	}
	if _, ok := top["prepaidMicros"]; !ok {
		return nil, "", invalid
	}
	var routes []map[string]json.RawMessage
	if json.Unmarshal(top["routes"], &routes) != nil || len(routes) != len(q.Routes) {
		return nil, "", invalid
	}
	result := *q
	result.Routes = append([]ImagePrepaymentRoute(nil), q.Routes...)
	result.PrepaidMicros = 0
	changed := false
	for i := range result.Routes {
		route := &result.Routes[i]
		var id string
		if routes[i] == nil || json.Unmarshal(routes[i]["routeId"], &id) != nil || id != route.RouteID {
			return nil, "", invalid
		}
		if hold, metered := holds[id]; metered {
			_, hasEstimate := routes[i]["estimatedMicros"]
			_, hasPrepaid := routes[i]["prepaidMicros"]
			if !hasEstimate || !hasPrepaid || hold < 1 || hold > route.PrepaidMicros {
				return nil, "", invalid
			}
			changed = changed || route.EstimatedMicros != hold || route.PrepaidMicros != hold
			route.EstimatedMicros, route.PrepaidMicros = hold, hold
			amount := json.RawMessage(strconv.FormatInt(hold, 10))
			routes[i]["estimatedMicros"], routes[i]["prepaidMicros"] = amount, amount
		}
		result.PrepaidMicros = max(result.PrepaidMicros, route.PrepaidMicros)
	}
	if !changed && result.PrepaidMicros == q.PrepaidMicros {
		return &result, encoded, nil
	}
	encodedRoutes, err := compactJSON(routes)
	if err != nil {
		return nil, "", invalid
	}
	top["routes"] = encodedRoutes
	top["prepaidMicros"] = json.RawMessage(strconv.FormatInt(result.PrepaidMicros, 10))
	rewritten, err := compactJSON(top)
	if err != nil {
		return nil, "", invalid
	}
	// The wallet re-checks the quote; prove here that it reads back as sized.
	parsed, err := ParseImagePrepaymentQuote(string(rewritten))
	if err != nil || !reflect.DeepEqual(*parsed, result) {
		return nil, "", invalid
	}
	return parsed, string(rewritten), nil
}

// compactJSON encodes without HTML escaping and without a trailing newline,
// so raw values keep their original text.
func compactJSON(value any) (json.RawMessage, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}
