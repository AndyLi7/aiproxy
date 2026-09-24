package model

import (
	"errors"
	"math/big"
)

// ImagePixelQuantity describes audited units, not provider-specific execution code.
type ImagePixelQuantity struct {
	PixelsPerUnit int64  `json:"pixelsPerUnit"`
	Rounding      string `json:"rounding"`
	Scope         string `json:"scope"`
	MinimumUnits  int64  `json:"minimumUnits"`
}

type ImageQuantityFraction struct {
	Numerator   string `json:"numerator"`
	Denominator string `json:"denominator"`
}

func (q ImagePixelQuantity) Validate() error {
	if !imageInteger(q.PixelsPerUnit, 1, ImageBillingMaxSafeInteger) ||
		!imageInteger(q.MinimumUnits, 0, ImageBillingMaxSafeInteger) ||
		(q.Rounding != "none" && q.Rounding != "ceil") ||
		(q.Scope != "per_image" && q.Scope != "per_request") {
		return errors.New("invalid pixel quantity")
	}
	return nil
}

// Admission uses the same unit math but rounds money upward for a conservative bound.
func maximumPixelBillingAmount(q *ImagePixelQuantity, rate ImageBillingRate, outputs, pixels int64) (float64, error) {
	divisor := big.NewInt(q.PixelsPerUnit)
	n := big.NewInt(pixels)
	if q.Scope == "per_request" {
		n.Mul(n, big.NewInt(outputs))
	}
	if q.Rounding == "ceil" {
		n.Add(n, new(big.Int).Sub(divisor, big.NewInt(1)))
		n.Quo(n, divisor)
		n.Mul(n, divisor)
	}
	minimum := new(big.Int).Mul(big.NewInt(q.MinimumUnits), divisor)
	if n.Cmp(minimum) < 0 {
		n.Set(minimum)
	}
	if q.Scope == "per_image" {
		n.Mul(n, big.NewInt(outputs))
	}
	denominator := new(big.Int).Mul(divisor, big.NewInt(rate.UnitQuantity))
	n.Mul(n, big.NewInt(rate.AmountMicros))
	n.Add(n, new(big.Int).Sub(denominator, big.NewInt(1)))
	n.Quo(n, denominator)
	if !n.IsInt64() || n.Int64() > ImageBillingMaxSafeInteger {
		return 0, errors.New("pixel budget overflow")
	}
	return float64(n.Int64()) / 1000000, nil
}

// Call only after policy, rate and complete output measurements have been validated.
func evaluatePixelBilling(q *ImagePixelQuantity, usage *ImageUsage, rate ImageBillingRate) (ImageBillingResult, error) {
	divisor := big.NewInt(q.PixelsPerUnit)
	minimum := new(big.Int).Mul(big.NewInt(q.MinimumUnits), divisor)
	measure := func(pixels *big.Int) *big.Int {
		n := new(big.Int).Set(pixels)
		if q.Rounding == "ceil" {
			n.Add(n, new(big.Int).Sub(divisor, big.NewInt(1)))
			n.Quo(n, divisor)
			n.Mul(n, divisor)
		}
		if n.Cmp(minimum) < 0 {
			n.Set(minimum)
		}
		return n
	}
	numerator := new(big.Int)
	indexes := make([]int64, len(usage.Outputs))
	for i, output := range usage.Outputs {
		pixels := new(big.Int).Mul(big.NewInt(*output.Width), big.NewInt(*output.Height))
		if q.Scope == "per_image" {
			pixels = measure(pixels)
		}
		numerator.Add(numerator, pixels)
		indexes[i] = output.Index
	}
	if q.Scope == "per_request" {
		numerator = measure(numerator)
	}
	denominator := new(big.Int).Mul(divisor, big.NewInt(rate.UnitQuantity))
	amount := new(big.Int).Mul(numerator, big.NewInt(rate.AmountMicros))
	amount.Mul(amount, big.NewInt(2))
	amount.Add(amount, denominator)
	amount.Quo(amount, new(big.Int).Mul(denominator, big.NewInt(2)))
	if !amount.IsInt64() || amount.Int64() > ImageBillingMaxSafeInteger {
		return ImageBillingResult{}, errors.New("image billing overflow")
	}
	micros := amount.Int64()
	quantity, _ := new(big.Rat).SetFrac(numerator, divisor).Float64()
	return ImageBillingResult{State: "complete", AmountMicros: &micros,
		Lines: []ImageBillingLine{{Kind: "output", Quantity: quantity,
			QuantityFraction: &ImageQuantityFraction{Numerator: numerator.String(), Denominator: divisor.String()},
			AmountMicros:     micros, Rate: rate, OutputIndexes: indexes,
		}},
	}, nil
}
