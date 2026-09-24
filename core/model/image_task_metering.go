package model

import (
	"errors"
	"math/big"
)

// QueueImageMaximumAmount is only an admission bound, never settlement evidence.
// A ceiling per output also covers the evaluator's separate tier rounding.
func QueueImageMaximumAmount(p Price, outputs int, inputs int64, maximumOutputPixels ...int64) (float64, error) {
	if p.ImageBilling == nil || p.ImageBilling.Scenario != "generation" ||
		len(p.ConditionalPrices) != 0 ||
		outputs < 1 ||
		outputs > ImageBillingMaxOutputs ||
		inputs < 0 {
		return 0, errors.New("unsupported queue image price")
	}

	if err := p.validateImageBillingPrice(); err != nil {
		return 0, err
	}

	rate, err := p.ImageBillingFallbackRate()
	if err != nil {
		return 0, err
	}

	if p.ImageBilling.Version == 2 {
		// Caller must derive this bound from a validated immutable Registry contract,
		// never an untrusted client estimate. Absence must not fall back to image count.
		if len(maximumOutputPixels) != 1 || !imageInteger(maximumOutputPixels[0], 1, ImageBillingMaxSafeInteger) {
			return 0, errors.New("pixel billing requires a verified output pixel bound")
		}
		return maximumPixelBillingAmount(p.ImageBilling.OutputPixels, rate, int64(outputs), maximumOutputPixels[0])
	}

	rates := []ImageBillingRate{rate}
	if p.ImageBilling.OutputPixelTiers != nil {
		rates = nil
		for _, tier := range p.ImageBilling.OutputPixelTiers {
			rates = append(rates, ImageBillingRate{tier.AmountMicros, tier.UnitQuantity})
		}
	}

	var maximum int64
	for _, r := range rates {
		q := new(
			big.Int,
		).Add(big.NewInt(r.AmountMicros), new(big.Int).Sub(big.NewInt(r.UnitQuantity), big.NewInt(1)))
		q.Quo(q, big.NewInt(r.UnitQuantity))

		if !q.IsInt64() || q.Int64() > ImageBillingMaxSafeInteger {
			return 0, errors.New("budget overflow")
		}

		if q.Int64() > maximum {
			maximum = q.Int64()
		}
	}

	policy := *p.ImageBilling
	policy.OutputPixelTiers = nil

	usage := &ImageUsage{
		Version:    1,
		State:      "complete",
		Scenario:   "generation",
		InputCount: &inputs,
		Outputs:    make([]ImageUsageOutput, outputs),
	}
	for i := range usage.Outputs {
		usage.Outputs[i].Index = int64(i)
	}

	result, err := EvaluateImageBilling(&policy, usage, ImageBillingRate{maximum, 1})
	if err != nil {
		return 0, err
	}

	if result.State != "complete" || result.AmountMicros == nil {
		return 0, errors.New("incomplete budget")
	}

	return float64(*result.AmountMicros) / 1000000, nil
}
