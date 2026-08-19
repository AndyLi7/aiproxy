package doubao

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRatioFromSizeRecognizesCanonicalRoundedVideoDimensions(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"854x480":  "16:9",
		"480x854":  "9:16",
		"1280x720": "16:9",
		"720x1280": "9:16",
		"480x480":  "1:1",
	}

	for size, expected := range tests {
		size, expected := size, expected
		t.Run(size, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, expected, ratioFromSize(size))
		})
	}
}

func TestDoubaoVideoDimensionsUsesCanonicalRoundedSixteenByNineSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		resolution string
		ratio      string
		wantWidth  int
		wantHeight int
	}{
		{resolution: "480p", ratio: "16:9", wantWidth: 854, wantHeight: 480},
		{resolution: "480p", ratio: "9:16", wantWidth: 480, wantHeight: 854},
		{resolution: "720p", ratio: "16:9", wantWidth: 1280, wantHeight: 720},
		{resolution: "1080p", ratio: "16:9", wantWidth: 1920, wantHeight: 1080},
	}

	for _, test := range tests {
		width, height := doubaoVideoDimensions(test.resolution, test.ratio)
		require.Equal(t, test.wantWidth, width)
		require.Equal(t, test.wantHeight, height)
	}
}
