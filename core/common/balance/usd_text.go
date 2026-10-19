package balance

import (
	"math"
	"strconv"
	"strings"
)

const (
	microsPerUSD  = 1_000_000
	microsPerCent = 10_000
	maxSafeMicros = 9_007_199_254_740_991
	// subCentOwedText is the amount owed when a balance is below zero by less
	// than one cent: it never reads as "-$0.00".
	subCentOwedText = "less than $0.01"
	// negativeBalanceAdvice is cause-neutral: a balance below zero can come from
	// a settlement above its hold, an admin adjustment or a legacy post-charge.
	negativeBalanceAdvice = "New requests are paused until your balance is back above zero; a top-up covers the amount owed first."
)

// MicrosText renders a micro-USD amount for customer messages: "$0.075",
// "$1.20", "$0.000123", "$1,250.00". It keeps at least 2 and at most 6
// decimals with trailing zeros trimmed, and groups thousands like the
// application's USD formatter.
func MicrosText(m int64) string {
	sign := ""

	var magnitude uint64
	if m < 0 {
		sign = "-"
		// #nosec G115 -- m is negative; -(m+1) is nonnegative, including MinInt64, so this conversion preserves its magnitude.
		magnitude = uint64(-(m + 1)) + 1
	} else {
		magnitude = uint64(m)
	}

	whole, fraction := magnitude/microsPerUSD, magnitude%microsPerUSD

	decimals := strings.TrimRight(strconv.FormatUint(fraction+microsPerUSD, 10)[1:], "0")
	for len(decimals) < 2 {
		decimals += "0"
	}

	return sign + "$" + groupThousands(strconv.FormatUint(whole, 10)) + "." + decimals
}

func groupThousands(digits string) string {
	if len(digits) <= 3 {
		return digits
	}

	head := len(digits) % 3
	if head == 0 {
		head = 3
	}

	var b strings.Builder
	b.WriteString(digits[:head])

	for i := head; i < len(digits); i += 3 {
		b.WriteByte(',')
		b.WriteString(digits[i : i+3])
	}

	return b.String()
}

// OwedText is the amount owed by a balance below zero (in USD), following the
// application's debt amount rule: rounded up to the cent ("$0.12"), and
// "less than $0.01" below one cent, so it never reads as "-$0.00".
func OwedText(balanceUSD float64) string {
	owedMicros := math.Round(-balanceUSD * microsPerUSD)
	if !(owedMicros >= microsPerCent) { // also NaN
		return subCentOwedText
	}
	// Wallet amounts are safe integers of micro-USD; clamp before converting.
	owedMicros = math.Min(owedMicros, maxSafeMicros)
	cents := int64(math.Ceil(owedMicros / microsPerCent))

	return MicrosText(cents * microsPerCent)
}

// NegativeBalanceMessage is the 402 message for a balance below zero:
// "Your balance is below zero (-$0.12). New requests are paused until your
// balance is back above zero; a top-up covers the amount owed first."
func NegativeBalanceMessage(balanceUSD float64) string {
	owed := OwedText(balanceUSD)
	if owed == subCentOwedText {
		return "Your balance is below zero (" + subCentOwedText + " owed). " + negativeBalanceAdvice
	}

	return "Your balance is below zero (-" + owed + "). " + negativeBalanceAdvice
}
