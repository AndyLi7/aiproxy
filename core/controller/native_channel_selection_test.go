//nolint:testpackage
package controller

import (
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

// A fresh /v1/model-tasks submission selects channels by mode.NativeTasks, so a
// published fal channel must stay eligible for that mode.
func TestNativeTaskAdmissionSelectsFalChannels(t *testing.T) {
	falChannel := &model.Channel{ID: 7, Type: model.ChannelTypeFal}
	mc := &model.ModelCaches{
		ChannelsByID: map[int]*model.Channel{7: falChannel},
		EnabledModel2ChannelsBySet: map[string]map[string][]*model.Channel{
			"default": {"vendor/native-model": {falChannel}},
		},
	}

	channels, err := getAvailableChannels(mc, []string{"default"}, "vendor/native-model", mode.NativeTasks)
	require.NoError(t, err)
	require.Len(t, channels, 1)
	require.Equal(t, 7, channels[0].ID)

	_, err = getAvailableChannels(mc, []string{"default"}, "vendor/native-model", mode.ChatCompletions)
	require.ErrorIs(t, err, ErrChannelsNotFound)
}
