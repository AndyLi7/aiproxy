package nativetask

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"gorm.io/gorm"
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

// FalQueueProvider is fal's entry in Engine.QueueProviders: queued tasks are
// sent through their frozen fal channel, and fal image tasks share the
// account's capacity (spec §5.3, §11.4).
func FalQueueProvider(load func(int) (*model.Channel, error)) QueueProvider {
	return QueueProvider{Resolve: ResolveFalDispatch(load), OtherInFlight: model.ImageTasksInFlight(model.ChannelTypeFal)}
}

// ResolveFalDispatch checks, when a queued task is dispatched, that its frozen
// channel is still enabled with the same type, host and key, and returns its
// client (spec §6.4). A disabled (including emergency-stopped), deleted,
// retargeted or rekeyed channel is ErrChannelUnusable; a failed read is not.
func ResolveFalDispatch(load func(int) (*model.Channel, error)) func(context.Context, *model.NativeTask) (Provider, error) {
	return func(_ context.Context, task *model.NativeTask) (Provider, error) {
		if load == nil || task == nil {
			return nil, ErrUnavailable
		}
		if task.ChannelID <= 0 || len(task.KeyFingerprint) != 64 {
			return nil, ErrChannelUnusable
		}
		channel, err := load(task.ChannelID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrChannelUnusable
		}
		if err != nil || channel == nil {
			return nil, ErrUnavailable
		}
		if channel.Status != model.ChannelStatusEnabled || channel.Type != model.ChannelTypeFal || channel.Key == "" ||
			(channel.BaseURL != "" && strings.TrimRight(channel.BaseURL, "/") != "https://queue.fal.run") {
			return nil, ErrChannelUnusable
		}
		current := model.ImageChannelKeyFingerprint(channel.Key)
		if subtle.ConstantTimeCompare([]byte(current), []byte(task.KeyFingerprint)) != 1 {
			return nil, ErrChannelUnusable
		}
		return &fal.Client{Key: channel.Key}, nil
	}
}
