package nativetask

import (
	"context"
	"crypto/subtle"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"strings"
)

// ResolveFalPoller binds recovery to the original account. Native execution v1
// uses the official queue host only; changing channel secrets never retargets a
// previously accepted request to a different provider account.
func ResolveFalPoller(load func(int) (*model.Channel, error)) ResolvePoller {
	return func(_ context.Context, task *model.NativeTask) (Poller, error) {
		if load == nil || task == nil || task.ChannelID <= 0 || len(task.KeyFingerprint) != 64 {
			return nil, ErrUnavailable
		}
		channel, err := load(task.ChannelID)
		if err != nil || channel == nil || channel.Type != model.ChannelTypeFal || channel.Key == "" {
			return nil, ErrUnavailable
		}
		if channel.BaseURL != "" && strings.TrimRight(channel.BaseURL, "/") != "https://queue.fal.run" {
			return nil, ErrUnavailable
		}
		current := model.ImageChannelKeyFingerprint(channel.Key)
		if subtle.ConstantTimeCompare([]byte(current), []byte(task.KeyFingerprint)) != 1 {
			return nil, ErrUnavailable
		}
		return &fal.Client{Key: channel.Key}, nil
	}
}

// Expired tasks are cancelled through the bound fal client; fail the build if it stops being a Canceller.
var _ Canceller = (*fal.Client)(nil)
