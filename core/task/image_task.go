package task

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/labring/aiproxy/core/common/consume"
	"github.com/labring/aiproxy/core/common/ownedimage"
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

	if task.Status == "result_processing" {
		if err := archiveImageTask(ctx, task, ownedimage.StoreWithMetadata); err != nil {
			retryImageUsage(info, err)
			return
		}
		completeImageTaskUsage(ctx, info, task.Data)
		return
	}
	if task.Status == "completed" {
		if len(task.Data) == 0 || len(task.Data) > model.ImageBillingMaxOutputs ||
			(task.ExpectedImages > 0 && len(task.Data) > task.ExpectedImages) {
			markAsyncUsageFailed(info, "unexpected stored image count")
			return
		}

		completeImageTaskUsage(ctx, info, task.Data)

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

	if result.Status == "completed" && task.ArchiveRequired {
		result.Status = "result_processing"
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

	// Another worker may have committed a terminal result before our update.
	// SetImageTaskResult leaves terminal rows unchanged, so dispatch accounting
	// from the durable winner for every poll outcome, including failed/queued.
	saved, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
	if err != nil {
		retryImageUsage(info, err)
		return
	}

	switch saved.Status {
	case "result_processing":
		if err := archiveImageTask(ctx, saved, ownedimage.StoreWithMetadata); err != nil {
			retryImageUsage(info, err)
			return
		}
		completeImageTaskUsage(ctx, info, saved.Data)
	case "completed":
		if len(saved.Data) == 0 || len(saved.Data) > model.ImageBillingMaxOutputs ||
			(task.ExpectedImages > 0 && len(saved.Data) > task.ExpectedImages) {
			markAsyncUsageFailed(info, "unexpected stored image count")
			return
		}

		completeImageTaskUsage(ctx, info, saved.Data)
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

func completeImageTaskUsage(
	ctx context.Context,
	info *model.AsyncUsageInfo,
	outputs []model.ImageOutput,
) {
	// Freeze the no-customer-billing decision at admission, independent of current
	// group status, credentials, release or retail price configuration.
	if info.InternalImageTask {
		info.Price = model.Price{}
		info.Amount = model.Amount{}
		info.MeasuredImage = false
	}

	usage := model.Usage{ImageOutputTokens: model.ZeroNullInt64(len(outputs))}

	usageContext := info.UsageContext
	if usageContext.ImageUsage != nil {
		evidence := *usageContext.ImageUsage
		evidence.State = "complete"
		count := int64(len(outputs))
		evidence.GeneratedCount = &count

		evidence.Outputs = make([]model.ImageUsageOutput, len(outputs))
		for i, out := range outputs {
			evidence.Outputs[i] = model.ImageUsageOutput{
				Index:  int64(i),
				Width:  out.Width,
				Height: out.Height,
			}
		}

		usageContext.ImageUsage = &evidence
	}

	if info.Price.HasImageBilling() && !info.MeasuredImage {
		amount := consume.CalculateMeasuredImageAmount(
			http.StatusOK,
			usageContext.ImageUsage,
			info.Price,
		)
		if err := model.PrepareClaimedImageTaskMeasurement(
			info,
			usage,
			usageContext,
			amount,
		); err != nil {
			retryImageUsage(info, err)
			return
		}

		info.MeasuredImage = true
		info.Usage = usage
		info.UsageContext = usageContext
		info.Amount = amount
	}

	if info.MeasuredImage &&
		(info.Amount.ImageBillingResult == nil || info.Amount.ImageBillingResult.State == "pending") {
		if err := model.ParkMeasuredImageUsage(info); err != nil {
			retryImageUsage(info, err)
		}
		return
	}

	completePolledAsyncUsage(ctx, info, usage, usageContext)
}

func archiveImageTask(ctx context.Context, task *model.ImageTask, store func(context.Context, string) (string, ownedimage.Metadata, error)) error {
	if len(task.Data) == 0 || len(task.Data) > model.ImageBillingMaxOutputs || (task.ExpectedImages > 0 && len(task.Data) > task.ExpectedImages) {
		return errors.New("invalid archive output count")
	}
	// Bound work per poll. Each completed output is durable even when a later
	// download/upload fails or the worker restarts.
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	for i, out := range task.Data {
		if out.Stored {
			continue
		}
		url, metadata, err := store(ownedimage.WithArchiveIdentity(ctx, task.ID, i), out.URL)
		if err != nil {
			return errors.New("image result storage temporarily unavailable")
		}
		out.URL = url
		out.Stored = true
		out.ContentType = metadata.ContentType
		if metadata.Width > 0 && metadata.Height > 0 {
			w, h := int64(metadata.Width), int64(metadata.Height)
			out.Width = &w
			out.Height = &h
		}
		if err := model.SaveArchivedImage(task, i, out); err != nil {
			return err
		}
	}
	return model.CompleteImageArchive(task)
}
