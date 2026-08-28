package meta

import (
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

func TestMetaMapsRoutingModelWithoutExposingItAsOriginModel(t *testing.T) {
	t.Parallel()

	requestMeta := NewMeta(
		&model.Channel{ModelMapping: map[string]string{
			"bytedance/seedance-2.0::text-to-video": "doubao-seedance-2-0-260128",
		}},
		mode.Videos,
		"bytedance/seedance-2.0",
		model.ModelConfig{},
		WithRoutingModel("bytedance/seedance-2.0::text-to-video"),
		WithVideoCapability("text-to-video"),
	)

	require.Equal(t, "bytedance/seedance-2.0", requestMeta.OriginModel)
	require.Equal(t, "bytedance/seedance-2.0::text-to-video", requestMeta.RoutingModel)
	require.Equal(t, "doubao-seedance-2-0-260128", requestMeta.ActualModel)
	require.Equal(t, "text-to-video", requestMeta.VideoCapability)
	require.Equal(t, requestMeta.RoutingModel, requestMeta.StoreModel())
}

func TestMetaRoutingModelDefaultsToOriginModel(t *testing.T) {
	t.Parallel()

	requestMeta := NewMeta(nil, mode.ChatCompletions, "gpt-5", model.ModelConfig{})

	require.Equal(t, "gpt-5", requestMeta.RoutingModel)
	require.Equal(t, "gpt-5", requestMeta.StoreModel())
}
