package model

import (
	"strconv"
	"strings"
)

const (
	videoBillingAlignment    = 16
	maxVideoBillingDimension = 16_384
	maxVideoBillingSeconds   = 3_600
)

// VerifiedDoubaoVideoBillableDimensions returns the macroblock-aligned dimensions
// only when they exactly explain the settled output-token count. This keeps
// provider billing details out of responses if the upstream formula changes.
func VerifiedDoubaoVideoBillableDimensions(
	size string,
	seconds int,
	outputTokens int64,
) (width, height int, ok bool) {
	if seconds <= 0 || seconds > maxVideoBillingSeconds || outputTokens <= 0 {
		return 0, 0, false
	}

	parts := strings.Split(strings.ToLower(strings.TrimSpace(size)), "x")
	if len(parts) != 2 {
		return 0, 0, false
	}

	requestedWidth, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || requestedWidth <= 0 || requestedWidth > maxVideoBillingDimension {
		return 0, 0, false
	}
	requestedHeight, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || requestedHeight <= 0 || requestedHeight > maxVideoBillingDimension {
		return 0, 0, false
	}

	width = alignVideoBillingDimension(requestedWidth)
	height = alignVideoBillingDimension(requestedHeight)
	frames := int64(24*seconds + 1)
	numerator := int64(width) * int64(height) * frames
	if numerator%1024 != 0 || outputTokens != numerator/1024 {
		return 0, 0, false
	}

	return width, height, true
}

func alignVideoBillingDimension(value int) int {
	return (value + videoBillingAlignment - 1) / videoBillingAlignment * videoBillingAlignment
}
