package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerifiedVideoBillableDimensionsRequireTokenFormulaMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		size         string
		seconds      int
		outputTokens int64
		wantWidth    int
		wantHeight   int
		wantOK       bool
	}{
		{name: "480p landscape", size: "854x480", seconds: 2, outputTokens: 19845, wantWidth: 864, wantHeight: 480, wantOK: true},
		{name: "1080p landscape", size: "1920x1080", seconds: 2, outputTokens: 99960, wantWidth: 1920, wantHeight: 1088, wantOK: true},
		{name: "480p portrait", size: "480x854", seconds: 2, outputTokens: 19845, wantWidth: 480, wantHeight: 864, wantOK: true},
		{name: "already aligned", size: "1280x720", seconds: 5, outputTokens: 108900, wantWidth: 1280, wantHeight: 720, wantOK: true},
		{name: "token formula mismatch", size: "854x480", seconds: 2, outputTokens: 19844, wantOK: false},
		{name: "overflow alias token count", size: "854x480", seconds: 2, outputTokens: 19845 + (1 << 54), wantOK: false},
		{name: "dimension above safety bound", size: "16385x1024", seconds: 2, outputTokens: 803600, wantOK: false},
		{name: "duration above safety bound", size: "1024x1024", seconds: 3601, outputTokens: 88499200, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			width, height, ok := VerifiedDoubaoVideoBillableDimensions(
				tt.size,
				tt.seconds,
				tt.outputTokens,
			)

			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.wantWidth, width)
			require.Equal(t, tt.wantHeight, height)
		})
	}
}
