package balance_test

import (
	"math"
	"testing"

	balanceapi "github.com/labring/aiproxy/core/common/balance"
	"github.com/stretchr/testify/require"
)

func TestMicrosText(t *testing.T) {
	for micros, want := range map[int64]string{
		0:             "$0.00",
		200:           "$0.0002",
		75_000:        "$0.075",
		80_000:        "$0.08",
		1:             "$0.000001",
		1_200_000:     "$1.20",
		1_215_000:     "$1.215",
		1_250_000_000: "$1,250.00",
		-120_000:      "-$0.12",
		math.MinInt64: "-$9,223,372,036,854.775808",
	} {
		require.Equal(t, want, balanceapi.MicrosText(micros), micros)
	}
}

// The amount owed follows the application's debt amount rule (formatOwedUsd):
// rounded up to the cent, "less than $0.01" below one cent, never "-$0.00".
func TestOwedText(t *testing.T) {
	for balance, want := range map[float64]string{
		-0.12:      "$0.12",
		-0.120001:  "$0.13",
		-0.01:      "$0.01",
		-0.010001:  "$0.02",
		-0.0099:    "less than $0.01",
		-0.0012:    "less than $0.01",
		-0.0000001: "less than $0.01",
		-12.5:      "$12.50",
		-1234.567:  "$1,234.57",
		math.NaN(): "less than $0.01",
	} {
		require.Equal(t, want, balanceapi.OwedText(balance), balance)
	}
}

func TestNegativeBalanceMessage(t *testing.T) {
	require.Equal(
		t,
		"Your balance is below zero (-$0.12). New requests are paused until your balance is back above zero; a top-up covers the amount owed first.",
		balanceapi.NegativeBalanceMessage(-0.12),
	)
	sub := balanceapi.NegativeBalanceMessage(-0.0012)
	require.Equal(
		t,
		"Your balance is below zero (less than $0.01 owed). New requests are paused until your balance is back above zero; a top-up covers the amount owed first.",
		sub,
	)
	require.NotContains(t, sub, "$0.00")
}
