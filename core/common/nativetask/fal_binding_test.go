package nativetask

import (
	"context"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNativeFalRecoveryPinsCredentialsAndHost(t *testing.T) {
	ch := &model.Channel{ID: 1, Type: model.ChannelTypeFal, Key: "original"}
	resolver := ResolveFalPoller(func(id int) (*model.Channel, error) { require.Equal(t, 1, id); return ch, nil })
	task := &model.NativeTask{ChannelID: 1, KeyFingerprint: model.ImageChannelKeyFingerprint("original")}
	provider, err := resolver(context.Background(), task)
	require.NoError(t, err)
	require.NotNil(t, provider)
	ch.Key = "rotated"
	_, err = resolver(context.Background(), task)
	require.Error(t, err)
	ch.Key = "original"
	ch.BaseURL = "https://other-account.example"
	_, err = resolver(context.Background(), task)
	require.Error(t, err)
	ch.BaseURL = "https://queue.fal.run/"
	_, err = resolver(context.Background(), task)
	require.NoError(t, err)
}
