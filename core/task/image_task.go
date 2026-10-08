package task

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/labring/aiproxy/core/common/consume"
	"github.com/labring/aiproxy/core/common/imageprepayment"
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
			handleImageArchiveFailure(info, task.ID, err)
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

	result, cancelUpstream, err := pollImageTask(ctx, info, task)
	// Owner rule 2026-10-08, for every model and provider: an accepted task
	// still waiting for the provider at model.AsyncGenerationDeadline fails and
	// is refunded. A result this final poll returned still wins; an error or an
	// unfinished status past the deadline does not.
	if (err != nil || (result.Status != "completed" && result.Status != "failed")) &&
		model.ImageGenerationExpired(task, time.Now()) {
		expireImageGeneration(ctx, info, task, cancelUpstream)
		return
	}
	if err != nil {
		retryImageUsageBefore(info, err, nextImageDeadlinePoll(task))
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
		result.Metadata,
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
			handleImageArchiveFailure(info, task.ID, err)
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

// imageCancel asks the provider once to stop an expired task. pollImageTask
// only returns one bound to the channel identity that passed its checks.
type imageCancel func(context.Context) (string, error)

// pollImageTask polls the provider with the credential that accepted the task.
// A rotated key, a retargeted channel or a missing adapter is an error, and
// then no cancel is offered, so another account's key is never used.
func pollImageTask(
	ctx context.Context,
	info *model.AsyncUsageInfo,
	task *model.ImageTask,
) (adaptor.ImageTaskResult, imageCancel, error) {
	ch, err := model.GetChannelByID(info.ChannelID)
	if err != nil {
		return adaptor.ImageTaskResult{}, nil, err
	}

	if task.KeyFingerprint != "" &&
		task.KeyFingerprint != model.ImageChannelKeyFingerprint(ch.Key) {
		return adaptor.ImageTaskResult{}, nil, errors.New("image channel credential changed")
	}

	channelType := task.ChannelType
	if channelType == 0 {
		channelType = ch.Type
	}

	if ch.Type != channelType {
		return adaptor.ImageTaskResult{}, nil, errors.New("image channel identity changed")
	}

	a, ok := adaptors.GetAdaptor(channelType)
	if !ok {
		return adaptor.ImageTaskResult{}, nil, errors.New("image adapter unavailable")
	}

	adapter, ok := a.(adaptor.ImageTaskAdapter)
	if !ok {
		return adaptor.ImageTaskResult{}, nil, errors.New("image adapter unavailable")
	}

	var cancelUpstream imageCancel
	if canceller, ok := a.(adaptor.ImageTaskCanceller); ok && task.UpstreamID != "" {
		cancelUpstream = func(ctx context.Context) (string, error) {
			return canceller.CancelImage(ctx, ch, info, task)
		}
	}

	pollCtx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()

	result, err := adapter.PollImage(pollCtx, ch, info, task)

	return result, cancelUpstream, err
}

const (
	// imageTimeoutRefundTimeout bounds the immediate refund of an expired task;
	// prepayment recovery retries it every minute after that.
	imageTimeoutRefundTimeout = 30 * time.Second
	// imageCancelTimeout bounds the one best-effort provider cancel.
	imageCancelTimeout = 10 * time.Second
)

// expireImageGeneration ends a task that outlived model.AsyncGenerationDeadline.
// The order is durable failure, refund, then one best-effort cancel: neither
// the refund nor the cancel can undo the failure, and the cancel never delays
// the refund. Only the call that made the failure refunds and cancels.
func expireImageGeneration(
	ctx context.Context,
	info *model.AsyncUsageInfo,
	task *model.ImageTask,
	cancelUpstream imageCancel,
) {
	expired, err := model.ExpireImageGeneration(task.ID, time.Now())
	if err != nil {
		retryImageUsage(info, err)
		return
	}

	if !expired {
		// A result was saved first; the next pass dispatches the durable state.
		touchAsyncUsagePollCursor(info)
		return
	}

	entry := log.WithField("task_id", task.ID).WithField("channel_id", info.ChannelID)
	entry.Warn("image task generation timed out")
	// Detached: claim renewal stops once the outbox is failed and cancels ctx,
	// and a shutting-down worker should still finish the refund and cancel.
	detached := context.WithoutCancel(ctx)

	if task.PrepaymentQuoteJSON != "" {
		refundCtx, cancel := context.WithTimeout(detached, imageTimeoutRefundTimeout)
		failed, err := model.GetImageTask(task.ID, task.GroupID, task.TokenID)
		if err == nil {
			_, err = imageprepayment.Sync(refundCtx, failed)
		}
		cancel()

		if err != nil {
			entry.WithError(err).Warn("image task generation timed out; refund deferred to prepayment recovery")
		}
	}

	if cancelUpstream == nil {
		entry.Warn("image task generation timed out; provider cancel unavailable")
	} else {
		cancelCtx, cancel := context.WithTimeout(detached, imageCancelTimeout)
		outcome, err := cancelUpstream(cancelCtx)
		cancel()

		if err != nil {
			entry.WithError(err).Warn("image task generation timed out; provider cancel failed")
		} else {
			entry.WithField("cancel", outcome).Info("image task generation timed out; provider cancel sent")
		}
	}

	markAsyncUsageFailed(info, "image generation timed out")
}

// nextImageDeadlinePoll is the latest next poll for a task still waiting for
// the provider: one second past its deadline, so a provider that keeps
// erroring is expired within seconds of the deadline, not a full backoff later.
// Zero means no bound.
func nextImageDeadlinePoll(task *model.ImageTask) time.Time {
	if !model.ImageTaskAwaitingProvider(task) {
		return time.Time{}
	}
	return model.ImageGenerationDeadline(task).Add(time.Second)
}

func retryImageUsage(info *model.AsyncUsageInfo, err error) {
	retryImageUsageBefore(info, err, time.Time{})
}

// retryImageUsageBefore backs off like retryImageUsage, but never schedules
// the next poll after latest (when set).
func retryImageUsageBefore(info *model.AsyncUsageInfo, err error, latest time.Time) {
	// Transient transport/storage errors must never discard accepted work or
	// restart a submission. Backoff is bounded; records remain recoverable.
	info.RetryCount++
	info.Error = err.Error()

	info.NextPollAt = time.Now().Add(model.AsyncUsageBackoffDelay(info.RetryCount))
	if !latest.IsZero() && latest.Before(info.NextPollAt) {
		info.NextPollAt = latest
	}
	if updateErr := model.RetryClaimedAsyncUsageInfo(info); updateErr != nil {
		log.WithError(updateErr).Warn("persist image task retry")
	}
}

func completeImageTaskUsage(
	ctx context.Context,
	info *model.AsyncUsageInfo,
	outputs []model.ImageOutput,
) {
	task, err := model.GetImageTask(info.ImageTaskID, info.GroupID, info.TokenID)
	if err != nil {
		retryImageUsage(info, err)
		return
	}
	if task.PrepaymentQuoteJSON != "" {
		receipt, err := imageprepayment.Sync(ctx, task)
		if err != nil {
			retryImageUsage(info, err)
			return
		}
		if receipt.ChargedMicros == nil {
			touchAsyncUsagePollCursor(info)
			return
		}
		// The D34 wallet has already finalized this exact operation. Keep usage/log
		// accounting, but never invoke legacy post-consumption for a prepaid task.
		info.BalanceConsumed = true
		info.Price = model.Price{}
		info.MeasuredImage = false
		info.Usage = model.Usage{ImageOutputTokens: model.ZeroNullInt64(len(outputs))}
		info.Amount = model.Amount{UsedAmount: float64(*receipt.ChargedMicros) / 1000000, ImageOutputAmount: float64(*receipt.ChargedMicros) / 1000000}
		completePolledAsyncUsage(ctx, info, info.Usage, info.UsageContext)
		return
	}
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
	for _, out := range task.Data {
		if !model.ValidAuxiliaryImages(out) {
			return errors.New("invalid auxiliary image")
		}
	}
	for i := range task.Data {
		out := model.CloneImageOutputs(task.Data[i : i+1])[0]
		archive := func(asset *model.ImageOutput, name string) error {
			assetCtx := ownedimage.WithArchiveIdentity(ctx, task.ID, i)
			if name != "" {
				assetCtx = ownedimage.WithAuxiliaryName(assetCtx, name)
			}
			url, metadata, err := store(assetCtx, asset.URL)
			if err != nil {
				if errors.Is(err, ownedimage.ErrTooLarge) {
					return ownedimage.ErrTooLarge
				}
				return errors.New("image result storage temporarily unavailable")
			}
			asset.URL, asset.Stored, asset.ContentType = url, true, metadata.ContentType
			if metadata.Width > 0 && metadata.Height > 0 {
				w, h := int64(metadata.Width), int64(metadata.Height)
				asset.Width, asset.Height = &w, &h
			}
			return nil
		}
		if !out.Stored {
			if err := archive(&out, ""); err != nil {
				return err
			}
			if err := model.SaveArchivedImage(task, i, out); err != nil {
				return err
			}
		}
		for name, asset := range out.AuxiliaryImages {
			if asset == nil || asset.Stored {
				continue
			}
			// Clone again after each checkpoint: mutating a shared map before the
			// CAS would change the expected persisted JSON and lose the checkpoint.
			out = model.CloneImageOutputs(task.Data[i : i+1])[0]
			if err := archive(out.AuxiliaryImages[name], name); err != nil {
				return err
			}
			if err := model.SaveArchivedImage(task, i, out); err != nil {
				return err
			}
		}
	}
	return model.CompleteImageArchive(task)
}

// Permanent size failures must terminate; transport/storage outages remain retryable.
func handleImageArchiveFailure(info *model.AsyncUsageInfo, taskID string, archiveErr error) {
	if !errors.Is(archiveErr, ownedimage.ErrTooLarge) {
		retryImageUsage(info, archiveErr)
		return
	}
	if err := model.SetImageTaskResult(taskID, "failed", nil, &model.ImageTaskError{Code: "archive_size_exceeded", Message: "Image result exceeds delivery size limit"}); err != nil {
		retryImageUsage(info, err)
		return
	}
	log.WithField("task_id", taskID).Warn("image delivery size limit exceeded")
}
