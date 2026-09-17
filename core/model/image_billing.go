package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
)

const (
	ImageBillingMaxSafeInteger int64 = 9007199254740991
	ImageBillingMaxTiers             = 64
	ImageBillingMaxOutputs           = 1024
	ImageBillingMaxDimension   int64 = 10000000
	ImageBillingMaxRules             = 256
)

type ImageBillingRate struct {
	AmountMicros int64 `json:"amountMicros"`
	UnitQuantity int64 `json:"unitQuantity"`
}

// UnmarshalJSON distinguishes explicit free rates from omitted/null monetary data.
func (r *ImageBillingRate) UnmarshalJSON(data []byte) error {
	if _, err := strictImageObject(
		data,
		[]string{"amountMicros", "unitQuantity"},
		[]string{"amountMicros", "unitQuantity"},
	); err != nil {
		return err
	}

	type plain ImageBillingRate

	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	candidate := ImageBillingRate(decoded)
	if err := candidate.Validate(); err != nil {
		return err
	}

	*r = candidate

	return nil
}

type ImageBillingInput struct {
	ChargeBasis  string `json:"chargeBasis,omitempty"`
	FirstNFree   int64  `json:"firstNFree"`
	AmountMicros int64  `json:"amountMicros"`
	UnitQuantity int64  `json:"unitQuantity"`
}
type ImageBillingTier struct {
	MaxPixels    *int64 `json:"maxPixels"`
	AmountMicros int64  `json:"amountMicros"`
	UnitQuantity int64  `json:"unitQuantity"`
}
type ImageBillingPolicy struct {
	Version          int                `json:"version"`
	Scenario         string             `json:"scenario"`
	Input            *ImageBillingInput `json:"input,omitempty"`
	OutputPixelTiers []ImageBillingTier `json:"outputPixelTiers,omitempty"`
}
type ImageUsageOutput struct {
	Index  int64  `json:"index"`
	Width  *int64 `json:"width,omitempty"`
	Height *int64 `json:"height,omitempty"`
	ZIndex *int64 `json:"z_index,omitempty"`
}
type ImageUsage struct {
	Version        int                `json:"version"`
	State          string             `json:"state"`
	Scenario       string             `json:"scenario"`
	InputCount     *int64             `json:"input_count,omitempty"`
	GeneratedCount *int64             `json:"generated_count,omitempty"`
	Outputs        []ImageUsageOutput `json:"outputs"`
}
type ImageBillingLine struct {
	Kind          string           `json:"kind"`
	TierIndex     *int             `json:"tierIndex"`
	Quantity      int64            `json:"quantity"`
	AmountMicros  int64            `json:"amountMicros"`
	Rate          ImageBillingRate `json:"rate"`
	OutputIndexes []int64          `json:"outputIndexes"`
}
type ImageBillingResult struct {
	State        string             `json:"state"`
	AmountMicros *int64             `json:"amountMicros"`
	Lines        []ImageBillingLine `json:"lines"`
	Reason       string             `json:"reason,omitempty"`
}

func imageInteger(n, minimum, maximum int64) bool { return n >= minimum && n <= maximum }

func imageScenario(
	s string,
) bool {
	return s == "generation" || s == "layer_decomposition"
}

func (r ImageBillingRate) Validate() error {
	if !imageInteger(r.AmountMicros, 0, ImageBillingMaxSafeInteger) ||
		!imageInteger(r.UnitQuantity, 1, ImageBillingMaxSafeInteger) {
		return errors.New("invalid image billing rate")
	}

	return nil
}

// strictImageObject additionally checks required fields: encoding/json otherwise silently accepts missing/null integers.
func strictImageObject(data []byte, keys, required []string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}

	if obj == nil {
		return nil, errors.New("expected object")
	}

	for k := range obj {
		found := slices.Contains(keys, k)

		if !found {
			return nil, fmt.Errorf("unknown image billing key %s", k)
		}
	}

	for _, k := range required {
		v, ok := obj[k]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return nil, fmt.Errorf("missing image billing key %s", k)
		}
	}

	return obj, nil
}

func (p *ImageBillingPolicy) UnmarshalJSON(data []byte) error {
	obj, err := strictImageObject(
		data,
		[]string{"version", "scenario", "input", "outputPixelTiers"},
		[]string{"version", "scenario"},
	)
	if err != nil {
		return err
	}

	if v, ok := obj["input"]; ok {
		var inputFields map[string]json.RawMessage
		if json.Unmarshal(v, &inputFields) != nil {
			return errors.New("invalid input billing")
		}

		if basis, exists := inputFields["chargeBasis"]; exists {
			var value string
			if json.Unmarshal(basis, &value) != nil ||
				(value != "per_request" && value != "per_output") {
				return errors.New("invalid input charge basis")
			}
		}

		if _, err = strictImageObject(
			v,
			[]string{"firstNFree", "amountMicros", "unitQuantity", "chargeBasis"},
			[]string{"firstNFree", "amountMicros", "unitQuantity"},
		); err != nil {
			return err
		}
	}

	if v, ok := obj["outputPixelTiers"]; ok {
		var ts []json.RawMessage
		if err = json.Unmarshal(v, &ts); err != nil {
			return err
		}

		if len(ts) == 0 || len(ts) > ImageBillingMaxTiers {
			return errors.New("invalid tier count")
		}

		for _, t := range ts {
			var to map[string]json.RawMessage

			to, err = strictImageObject(
				t,
				[]string{"maxPixels", "amountMicros", "unitQuantity"},
				[]string{"amountMicros", "unitQuantity"},
			)
			if err != nil {
				return err
			}

			if _, ok := to["maxPixels"]; !ok {
				return errors.New("missing maxPixels")
			}
		}
	}

	type plain ImageBillingPolicy

	var decoded plain
	if err = json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	candidate := ImageBillingPolicy(decoded)
	if err = candidate.Validate(); err != nil {
		return err
	}

	*p = candidate

	return nil
}

func (p *ImageBillingPolicy) Validate() error {
	if p == nil || p.Version != 1 || !imageScenario(p.Scenario) {
		return errors.New("invalid image billing policy")
	}

	if p.Input != nil {
		if p.Input.ChargeBasis != "" && p.Input.ChargeBasis != "per_request" &&
			p.Input.ChargeBasis != "per_output" {
			return errors.New("invalid input charge basis")
		}

		if !imageInteger(p.Input.FirstNFree, 0, ImageBillingMaxSafeInteger) {
			return errors.New("invalid allowance")
		}

		if err := (ImageBillingRate{p.Input.AmountMicros, p.Input.UnitQuantity}).Validate(); err != nil {
			return err
		}
	}

	if p.OutputPixelTiers != nil {
		if len(p.OutputPixelTiers) == 0 || len(p.OutputPixelTiers) > ImageBillingMaxTiers {
			return errors.New("invalid tier count")
		}

		var previous int64
		for i, t := range p.OutputPixelTiers {
			if (i == len(p.OutputPixelTiers)-1) != (t.MaxPixels == nil) {
				return errors.New("final tier must be open")
			}

			if t.MaxPixels != nil {
				if !imageInteger(*t.MaxPixels, 1, ImageBillingMaxSafeInteger) ||
					*t.MaxPixels <= previous {
					return errors.New("tiers must increase")
				}

				previous = *t.MaxPixels
			}

			if err := (ImageBillingRate{t.AmountMicros, t.UnitQuantity}).Validate(); err != nil {
				return err
			}
		}
	}

	return nil
}

func (u *ImageUsage) UnmarshalJSON(data []byte) error {
	obj, err := strictImageObject(
		data,
		[]string{"version", "state", "scenario", "input_count", "generated_count", "outputs"},
		[]string{"version", "state", "scenario"},
	)
	if err != nil {
		return err
	}
	// Failure does not require usable output dimensions, counts or identities.
	var state string
	if err = json.Unmarshal(obj["state"], &state); err != nil {
		return err
	}

	if state == "failed" {
		var head struct {
			Version  int    `json:"version"`
			State    string `json:"state"`
			Scenario string `json:"scenario"`
		}
		if err = json.Unmarshal(data, &head); err != nil {
			return err
		}

		*u = ImageUsage{Version: head.Version, State: head.State, Scenario: head.Scenario}

		return nil
	}

	if _, ok := obj["outputs"]; !ok ||
		bytes.Equal(bytes.TrimSpace(obj["outputs"]), []byte("null")) {
		return errors.New("missing outputs")
	}

	for _, k := range []string{"input_count", "generated_count"} {
		if v, ok := obj[k]; ok && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return errors.New("null count")
		}
	}

	var outputs []json.RawMessage
	if err = json.Unmarshal(obj["outputs"], &outputs); err != nil {
		return err
	}

	if len(outputs) > ImageBillingMaxOutputs {
		return errors.New("too many outputs")
	}

	for _, o := range outputs {
		var output map[string]json.RawMessage

		output, err = strictImageObject(
			o,
			[]string{"index", "width", "height", "z_index"},
			[]string{"index"},
		)
		if err != nil {
			return err
		}

		for _, k := range []string{"width", "height", "z_index"} {
			if v, ok := output[k]; ok && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return errors.New("null measurement")
			}
		}
	}

	type plain ImageUsage

	var decoded plain
	if err = json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	*u = ImageUsage(decoded)

	return nil
}

//nolint:gocyclo // Keep the shared cross-language evidence validation checks in one auditable pass.
func (u *ImageUsage) Validate(requirePixels bool) error {
	if u == nil || u.Version != 1 || !imageScenario(u.Scenario) ||
		(u.State != "complete" && u.State != "incomplete" && u.State != "failed") {
		return errors.New("invalid usage")
	}

	if u.State == "failed" {
		return nil
	}

	for _, n := range []*int64{u.InputCount, u.GeneratedCount} {
		if n != nil && !imageInteger(*n, 0, ImageBillingMaxSafeInteger) {
			return errors.New("invalid count")
		}
	}

	if u.Outputs == nil || len(u.Outputs) > ImageBillingMaxOutputs {
		return errors.New("missing outputs")
	}

	if u.GeneratedCount != nil && *u.GeneratedCount != int64(len(u.Outputs)) {
		return errors.New("count mismatch")
	}

	indexes := map[int64]bool{}

	layers := map[int64]bool{}
	for _, o := range u.Outputs {
		if !imageInteger(o.Index, 0, ImageBillingMaxSafeInteger) || indexes[o.Index] {
			return errors.New("invalid output index")
		}

		indexes[o.Index] = true
		for _, d := range []*int64{o.Width, o.Height} {
			if requirePixels && d == nil {
				return errors.New("missing dimensions")
			}

			if d != nil && !imageInteger(*d, 1, ImageBillingMaxDimension) {
				return errors.New("invalid dimensions")
			}
		}

		if o.Width != nil && o.Height != nil && *o.Width > ImageBillingMaxSafeInteger / *o.Height {
			return errors.New("pixel overflow")
		}

		if u.Scenario == "layer_decomposition" && o.ZIndex == nil {
			return errors.New("missing layer index")
		}

		if o.ZIndex != nil {
			if !imageInteger(*o.ZIndex, 0, ImageBillingMaxSafeInteger) || layers[*o.ZIndex] {
				return errors.New("invalid layer index")
			}

			layers[*o.ZIndex] = true
		}
	}

	if u.Scenario == "layer_decomposition" && len(u.Outputs) > 0 && !layers[0] {
		return errors.New("missing base image")
	}

	return nil
}

// EvaluateImageBilling returns a detached snapshot: it never retains policy or usage pointers/slices.
// Callers must persist the result and usage before settlement. Pending is not zero-priced success.
//
//nolint:gocyclo // Billing states and tier selection are kept in one auditable calculation.
func EvaluateImageBilling(
	policy *ImageBillingPolicy,
	usage *ImageUsage,
	defaultRate ImageBillingRate,
) (ImageBillingResult, error) {
	if err := policy.Validate(); err != nil {
		return ImageBillingResult{}, err
	}

	if policy.OutputPixelTiers == nil {
		if err := defaultRate.Validate(); err != nil {
			return ImageBillingResult{}, err
		}
	}

	terminal := func(state, reason string) ImageBillingResult {
		r := ImageBillingResult{State: state, Reason: reason, Lines: []ImageBillingLine{}}
		if state != "pending" {
			zero := int64(0)
			r.AmountMicros = &zero
		}

		return r
	}
	if err := usage.Validate(policy.OutputPixelTiers != nil); err != nil {
		return terminal("pending", "invalid_usage"), nil
	}

	if usage.Scenario != policy.Scenario {
		return terminal("pending", "scenario_mismatch"), nil
	}

	if usage.State == "failed" {
		return terminal("failed", ""), nil
	}

	if usage.State != "complete" {
		return terminal("pending", "incomplete_usage"), nil
	}

	if len(usage.Outputs) == 0 {
		return terminal("complete", ""), nil
	}

	if policy.Input != nil && policy.Input.AmountMicros > 0 && usage.InputCount == nil {
		return terminal("pending", "missing_input_count"), nil
	}

	result := terminal("complete", "")

	var total int64

	add := func(kind string, tier *int, quantity int64, rate ImageBillingRate, indexes []int64) error {
		n := new(big.Int).Mul(big.NewInt(quantity), big.NewInt(rate.AmountMicros))
		n.Mul(n, big.NewInt(2))
		n.Add(n, big.NewInt(rate.UnitQuantity))
		n.Quo(n, new(big.Int).Mul(big.NewInt(rate.UnitQuantity), big.NewInt(2)))

		if !n.IsInt64() || n.Int64() > ImageBillingMaxSafeInteger-total {
			return errors.New("image billing overflow")
		}

		amount := n.Int64()
		total += amount
		result.Lines = append(
			result.Lines,
			ImageBillingLine{
				Kind:          kind,
				TierIndex:     tier,
				Quantity:      quantity,
				AmountMicros:  amount,
				Rate:          rate,
				OutputIndexes: indexes,
			},
		)

		return nil
	}
	if in := policy.Input; in != nil {
		var q int64
		if usage.InputCount != nil && *usage.InputCount > in.FirstNFree {
			q = *usage.InputCount - in.FirstNFree
		}

		if in.ChargeBasis == "per_output" {
			n := int64(len(usage.Outputs))
			if n > 0 && q > ImageBillingMaxSafeInteger/n {
				return ImageBillingResult{}, errors.New("image billing quantity overflow")
			}

			q *= n
		}

		if err := add(
			"input",
			nil,
			q,
			ImageBillingRate{in.AmountMicros, in.UnitQuantity},
			[]int64{},
		); err != nil {
			return ImageBillingResult{}, err
		}
	}

	if policy.OutputPixelTiers == nil {
		indexes := make([]int64, len(usage.Outputs))
		for i, o := range usage.Outputs {
			indexes[i] = o.Index
		}

		if err := add("output", nil, int64(len(indexes)), defaultRate, indexes); err != nil {
			return ImageBillingResult{}, err
		}
	} else {
		for i, t := range policy.OutputPixelTiers {
			indexes := []int64{}
			for _, o := range usage.Outputs {
				pixels := *o.Width * *o.Height
				if (i == 0 || pixels > *policy.OutputPixelTiers[i-1].MaxPixels) &&
					(t.MaxPixels == nil || pixels <= *t.MaxPixels) {
					indexes = append(indexes, o.Index)
				}
			}

			if len(indexes) > 0 {
				tier := i
				if err := add(
					"output",
					&tier,
					int64(len(indexes)),
					ImageBillingRate{t.AmountMicros, t.UnitQuantity},
					indexes,
				); err != nil {
					return ImageBillingResult{}, err
				}
			}
		}
	}

	result.AmountMicros = &total

	return result, nil
}

// HasImageBilling is also used before response-time conditional selection.

func (p Price) HasImageBilling() bool {
	if p.ImageBilling != nil {
		return true
	}

	for _, branch := range p.ConditionalPrices {
		if branch.Price.ImageBilling != nil {
			return true
		}
	}

	return false
}

func (p Price) hasLegacyImageRate() bool {
	return p.OutputPrice != 0 || p.ImageOutputPrice != 0 || p.PerRequestPrice != 0
}

func (p Price) validateImageBillingPrice() error {
	if p.ImageBilling == nil {
		return nil
	}

	if err := p.ImageBilling.Validate(); err != nil {
		return err
	}

	if p.PerRequestPrice != 0 || p.InputPrice != 0 || p.ImageInputPrice != 0 ||
		p.AudioInputPrice != 0 ||
		p.VideoInputPrice != 0 ||
		p.AudioOutputPrice != 0 ||
		p.ThinkingModeOutputPrice != 0 ||
		p.CachedPrice != 0 ||
		p.CacheCreationPrice != 0 ||
		p.WebSearchPrice != 0 {
		return errors.New("measured image policy cannot mix legacy metrics")
	}

	if p.OutputPrice < 0 || p.ImageOutputPrice < 0 || p.OutputPriceUnit < 0 ||
		p.ImageOutputPriceUnit < 0 {
		return errors.New("invalid image fallback rate")
	}

	if p.OutputPrice != 0 && p.ImageOutputPrice != 0 {
		return errors.New("ambiguous image fallback rates")
	}

	_, err := p.ImageBillingFallbackRate()

	return err
}
