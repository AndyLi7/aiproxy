package controller

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/labring/aiproxy/core/common/imagecapabilities"
	"github.com/labring/aiproxy/core/common/imageprepayment"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/adaptors"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
)

func dispatchImageTaskWithFailover(c *gin.Context, task *model.ImageTask, executor adaptor.ImageTaskExecutor, mt *meta.Meta, body []byte) {
	if len(task.Attempts) > 0 {
		c.JSON(202, task)
		return
	}
	started := time.Now()
	retries, deadline := getRetryLimits(middleware.GetModelConfig(c), config.GetRetryTimes(), config.GetRetryBudget(), started)
	policy := failover.Policy{MaxRetries: retries, Deadline: deadline}
	state := failover.State{}
	routing := &retryState{meta: mt, ignoreChannelIDs: map[int64]struct{}{}, failedChannelIDs: map[int64]struct{}{}}
	if value, ok := c.Get("image_initial_channel"); ok {
		initial := value.(*initialChannel)
		routing.channelSelectionState = initial.channelSelectionState
		routing.migratedChannels = initial.migratedChannels
		routing.preferChannelIDs = initial.preferChannelIDs
		for id := range initial.ignoreChannelIDs {
			routing.ignoreChannelIDs[id] = struct{}{}
		}
		state.Pinned = initial.designatedChannel
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 50*time.Second)
	defer cancel()
	for {
		expected := len(task.Attempts)
		if task.PrepaymentQuoteJSON != "" {
			claimed, err := imageprepayment.Begin(ctx, task, mt.Channel.ID, mt.ActualModel, expected)
			if err != nil || !claimed {
				c.JSON(202, publicImageTask(c, task))
				return
			}
		}
		task.Attempts = append(task.Attempts, model.ImageTaskAttempt{ChannelID: mt.Channel.ID, StartedAt: time.Now(), Failure: failover.Failure{Acceptance: failover.Unknown, Class: failover.UnknownFailure}, Decision: "in_flight", PotentialCost: "unverified"})
		if model.SaveImageTaskAttempt(task, expected, mt.Channel.ID, mt.Channel.BaseURL) != nil {
			imageTaskHTTPError(c, 503, "task_store_unavailable")
			return
		}
		routing.ignoreChannelIDs[int64(mt.Channel.ID)] = struct{}{}
		var result adaptor.ImageTaskResult
		var upstream string
		var err error
		syncAdapter, synchronous := executor.(adaptor.SyncImageTaskAdapter)
		if synchronous {
			result, err = syncAdapter.GenerateImage(ctx, mt, body, []byte(task.ValidationContract))
		} else if queue, ok := executor.(adaptor.ImageTaskAdapter); ok {
			upstream, err = queue.SubmitImage(ctx, mt, body)
		} else {
			err = adaptor.ErrImageSubmissionRejected
		}
		failure := failover.FromError(err)
		if err == nil {
			failure = failover.Failure{Acceptance: failover.Accepted, Class: failover.UnknownFailure, Evidence: "provider_result"}
		}
		state.Cancelled = c.Request.Context().Err() != nil || ctx.Err() != nil
		state.Written = c.Writer.Written()
		decision := failover.Decide(failure, policy, state, time.Now())
		attempt := &task.Attempts[len(task.Attempts)-1]
		attempt.Failure = failure
		attempt.DurationMS = time.Since(attempt.StartedAt).Milliseconds()
		attempt.Decision = decision.Reason
		attempt.Retry = decision.Retry
		if failure.Acceptance == failover.NotAccepted {
			attempt.PotentialCost = "none"
		}
		if model.SaveImageTaskAttempt(task, len(task.Attempts), mt.Channel.ID, mt.Channel.BaseURL) != nil {
			imageTaskHTTPError(c, 503, "task_store_unavailable")
			return
		}
		if task.PrepaymentQuoteJSON != "" && failure.Acceptance == failover.NotAccepted {
			if err := imageprepayment.Reject(ctx, task, expected); err != nil {
				c.JSON(202, publicImageTask(c, task))
				return
			}
		}
		if err == nil {
			if synchronous {
				finishSyncImageTask(c, task, result, nil)
				return
			}
			if model.AcceptImageTask(task.ID, upstream) != nil {
				imageTaskHTTPError(c, 503, "task_store_unavailable")
				return
			}
			task.Status = "queued"
			task.UpstreamID = upstream
			if task.PrepaymentQuoteJSON != "" {
				_, _ = imageprepayment.Sync(ctx, task)
			}
			c.JSON(202, task)
			return
		}
		if decision.Retry {
			next, nextExecutor, nextMeta, nextBody := nextImageTaskCandidate(c, ctx, task, routing)
			if next != nil {
				// Mapping/selection may consume the remaining deadline; check again before submission.
				state.Cancelled = c.Request.Context().Err() != nil || ctx.Err() != nil
				decision = failover.Decide(failure, policy, state, time.Now())
				if decision.Retry {
					executor = nextExecutor
					mt = nextMeta
					body = nextBody
					state.Retries++
					continue
				}
			} else {
				decision = failover.Decision{Reason: "no_candidates"}
			}
			attempt.Decision = decision.Reason
			attempt.Retry = false
			// Candidate preparation may have changed the in-memory route. The last
			// attempted channel remains authoritative if no new call is started.
			var persisted model.ImageTask
			if model.LogDB.First(&persisted, "id = ?", task.ID).Error != nil {
				imageTaskHTTPError(c, 503, "task_store_unavailable")
				return
			}
			persisted.Attempts = task.Attempts
			*task = persisted
			if model.SaveImageTaskAttempt(task, len(task.Attempts), mt.Channel.ID, mt.Channel.BaseURL) != nil {
				imageTaskHTTPError(c, 503, "task_store_unavailable")
				return
			}
		}
		status := "submission_unknown"
		var taskError *model.ImageTaskError
		if failure.Acceptance == failover.NotAccepted {
			status = "failed"
			taskError = &model.ImageTaskError{Code: "submission_rejected", Message: "Upstream rejected image submission"}
			var detail *adaptor.ImageSubmissionFailure
			if errors.As(err, &detail) && detail.PublicError != nil {
				taskError = detail.PublicError
			}
		}
		if model.SetImageTaskResult(task.ID, status, nil, taskError) != nil {
			imageTaskHTTPError(c, 503, "task_store_unavailable")
			return
		}
		task.Status = status
		task.Error = taskError
		c.JSON(202, task)
		return
	}
}

func nextImageTaskCandidate(c *gin.Context, ctx context.Context, task *model.ImageTask, routing *retryState) (*model.Channel, adaptor.ImageTaskExecutor, *meta.Meta, []byte) {
	contract := imageProviderContract(c)
	original, err := common.GetRequestBodyReusable(c.Request)
	if err != nil {
		return nil, nil, nil, nil
	}
	var input struct {
		N int `json:"n"`
	}
	if json.Unmarshal(original, &input) != nil || input.N < 1 {
		return nil, nil, nil, nil
	}
	var usage model.AsyncUsageInfo
	if model.LogDB.First(&usage, task.UsageID).Error != nil {
		return nil, nil, nil, nil
	}
	for {
		channel, err := getRetryChannel(ctx, routing)
		if err != nil || channel == nil {
			return nil, nil, nil, nil
		}
		routing.ignoreChannelIDs[int64(channel.ID)] = struct{}{}
		raw, ok := adaptors.GetAdaptor(channel.Type)
		if !ok {
			continue
		}
		executor, ok := raw.(adaptor.ImageTaskExecutor)
		if !ok {
			continue
		}
		mc := middleware.GetModelConfig(c)
		if mc.Price.HasImageBilling() && !imagecapabilities.SupportsMeasuredBilling(executor.ImageAdapterName()) {
			continue
		}
		var body []byte
		frozen := contract
		if registryvalidation.HasProviderContracts(contract) {
			var binding registryvalidation.ProviderBinding
			body, binding, err = mapChannelProviderInput(contract, channel, middleware.GetRoutingModel(c), original)
			if err != nil {
				continue
			}
			frozen, err = registryvalidation.FreezeProviderBinding(contract, binding)
			if err != nil {
				continue
			}
			metering, e := registryvalidation.ResolveImageMetering(contract, binding, body, input.N, model.ImageBillingMaxOutputs, mc.Price.HasImageBilling())
			pixelLimit, pixelErr := registryvalidation.FrozenImagePixelLimit([]byte(task.ValidationContract))
			if pixelErr != nil || pixelLimit != metering.MaxOutputPixels {
				continue
			}
			// Do not change the reserved customer quantity or input metering on failover.
			if e != nil || !compatibleImageFailoverMetering(metering, task.ExpectedImages, usage.UsageContext.ImageUsage, mc.Price.HasImageBilling()) {
				continue
			}
		} else {
			if _, sync := executor.(adaptor.SyncImageTaskAdapter); sync {
				continue
			}
			body, err = adaptor.MapImageProviderInput(mc.Config, executor.ImageAdapterName(), original)
			if err != nil {
				continue
			}
		}
		mt := NewMetaByContext(c, channel, mode.ImagesGenerations)
		if task.PrepaymentQuoteJSON != "" {
			q, err := model.ParseImagePrepaymentQuote(task.PrepaymentQuoteJSON)
			if err != nil || q.Route(channel.ID, mt.ActualModel) == nil {
				continue
			}
		}
		task.ValidationContract = string(frozen)
		task.UpstreamModel = mt.ActualModel
		task.ChannelType = mt.Channel.Type
		task.KeyFingerprint = model.ImageChannelKeyFingerprint(mt.Channel.Key)
		return channel, executor, mt, body
	}
}

// Fixed per-image prices can bridge explicit zero-input evidence and implicit
// zero-input evidence. Measured pricing must retain its evidence requirements.
func compatibleImageFailoverMetering(next registryvalidation.ImageMeteringEvidence, expected int, previous *model.ImageUsage, measured bool) bool {
	if next.MaximumOutputs != expected {
		return false
	}
	if measured && next.Explicit != (previous != nil) {
		return false
	}
	if previous == nil {
		return next.InputCount == 0
	}
	if previous.InputCount == nil {
		return false
	}
	return *previous.InputCount == next.InputCount
}
