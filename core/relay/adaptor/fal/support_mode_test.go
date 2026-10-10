package fal_test

import (
	"testing"

	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

// Durable image tasks and native model tasks both run on fal channels; fal never
// serves the synchronous OpenAI-compatible relay modes.
func TestFalChannelsServeImageAndNativeTaskModesOnly(t *testing.T) {
	adaptor := &fal.Adaptor{}
	for m, supported := range map[mode.Mode]bool{
		mode.ImagesGenerations: true,
		mode.NativeTasks:       true,
		mode.ImagesEdits:       false,
		mode.ChatCompletions:   false,
	} {
		require.Equal(t, supported, adaptor.SupportMode(&meta.Meta{Mode: m}), m.String())
	}
}
