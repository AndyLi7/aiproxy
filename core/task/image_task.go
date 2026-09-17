package task

import (
	"context"
	"errors"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/adaptors"
	log "github.com/sirupsen/logrus"
)

// Image polling uses the existing renewable accounting lease. A validated result
// is persisted before settlement; a settlement retry never calls fal again.
func processOneImageUsage(ctx context.Context, info *model.AsyncUsageInfo) {
	task, err := model.GetImageTask(info.ImageTaskID, info.GroupID, info.TokenID)
	if err != nil {
		retryImageUsage(info, err)
		return
	}

	if task.Status == "completed" {
		completePolledAsyncUsage(
			ctx,
			info,
			model.Usage{ImageOutputTokens: model.ZeroNullInt64(len(task.Data))},
			info.UsageContext,
		)

		return
	}

	if task.Status == "failed" {
		markAsyncUsageFailed(info, "image generation failed")
		return
	}

	ch, err := model.GetChannelByID(info.ChannelID)
	if err != nil {
		retryImageUsage(info, err)
		return
	}

	if task.KeyFingerprint != "" &&
		task.KeyFingerprint != model.ImageChannelKeyFingerprint(ch.Key) {
		retryImageUsage(info, errors.New("image channel credential changed"))
		return
	}

	channelType := task.ChannelType
	if channelType == 0 {
		channelType = ch.Type
	}

	if ch.Type != channelType {
		retryImageUsage(info, errors.New("image channel identity changed"))
		return
	}

	a, ok := adaptors.GetAdaptor(channelType)
	if !ok {
		retryImageUsage(info, errors.New("image adapter unavailable"))
		return
	}

	adapter, ok := a.(adaptor.ImageTaskAdapter)
	if !ok {
		retryImageUsage(info, errors.New("image adapter unavailable"))
		return
	}

	pollCtx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()

	result, err := adapter.PollImage(pollCtx, ch, info, task)
	if err != nil {
		retryImageUsage(info, err)
		return
	}

	if result.Status == "completed" && task.ExpectedImages > 0 &&
		len(result.Data) > task.ExpectedImages {
		result = adaptor.ImageTaskResult{
			Status: "failed",
			Error: &model.ImageTaskError{
				Code:    "invalid_result",
				Message: "Unexpected image count",
			},
		}
	}

	if err = model.SetImageTaskResult(
		task.ID,
		result.Status,
		result.Data,
		result.Error,
	); err != nil {
		retryImageUsage(info, err)
		return
	}

	switch result.Status {
	case "completed":
		completePolledAsyncUsage(
			ctx,
			info,
			model.Usage{ImageOutputTokens: model.ZeroNullInt64(len(result.Data))},
			info.UsageContext,
		)
	case "failed":
		markAsyncUsageFailed(info, "image generation failed")
	default:
		touchAsyncUsagePollCursor(info)
	}
}

func retryImageUsage(info *model.AsyncUsageInfo, err error) {
	// Transient transport/storage errors must never discard accepted work or
	// restart a submission. Backoff is bounded; records remain recoverable.
	info.RetryCount++
	info.Error = err.Error()

	info.NextPollAt = time.Now().Add(model.AsyncUsageBackoffDelay(info.RetryCount))
	if updateErr := model.RetryClaimedAsyncUsageInfo(info); updateErr != nil {
		log.WithError(updateErr).Warn("persist image task retry")
	}
}
