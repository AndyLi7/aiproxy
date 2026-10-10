package controller

import (
	"context"
	"encoding/json"
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
	"github.com/sirupsen/logrus"
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
				entry := common.GetLogger(c).WithFields(logrus.Fields{"lane": "image", "task_id": task.ID, "channel_id": mt.Channel.ID, "attempt": expected + 1, "claimed": claimed})
				if err != nil {
					entry = entry.WithError(err)
				}
				entry.Warn("image task wallet attempt not claimed; nothing submitted")
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
		if err != nil {
			logImageSubmissionFailure(c, task, mt, err, failure, decision)
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
			if next != nil && task.PrepaymentQuoteJSON != "" {
				// The wallet admits the next channel's attempt only after this
				// one is released, so release it before switching. Only a retry
				// releases it here; a task that stops is persisted as failed
				// first and Sync below releases and settles it. If the wallet
				// cannot release it now, no other channel can be claimed: stop
				// and fail the task like one without a candidate.
				if rejectErr := imageprepayment.Reject(ctx, task, expected); rejectErr != nil {
					common.GetLogger(c).WithFields(logrus.Fields{"lane": "image", "task_id": task.ID, "channel_id": mt.Channel.ID, "next_channel_id": next.ID}).
						WithError(rejectErr).Warn("image task not-accepted attempt not released in the wallet; failover stopped and the task fails")
					next = nil
				}
			}
			if next != nil {
				// Mapping/selection and the wallet release may consume the
				// remaining deadline; check again before submission.
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
			taskError = adaptor.NotAcceptedTaskError(err)
		}
		if model.SetImageTaskResult(task.ID, status, nil, taskError) != nil {
			imageTaskHTTPError(c, 503, "task_store_unavailable")
			return
		}
		task.Status = status
		task.Error = taskError
		if status == "failed" && task.PrepaymentQuoteJSON != "" {
			// The provider created nothing and the failure is already
			// persisted: release the attempt (reject_attempt) and refund the
			// hold now. Prepayment recovery replays the same idempotent calls
			// for a failed, unsettled task if the wallet is down.
			if _, err := imageprepayment.Sync(ctx, task); err != nil {
				common.GetLogger(c).WithFields(logrus.Fields{"lane": "image", "task_id": task.ID}).
					WithError(err).Warn("image task not-accepted refund deferred to prepayment recovery")
			}
		}
		c.JSON(202, task)
		return
	}
}

// logImageSubmissionFailure records every failed provider submission for
// operators (owner rule 2026-10-08): identities, the provider status, the
// adaptor's sanitized reason and the failover decision. Never credentials,
// request bodies or the customer's prompt.
func logImageSubmissionFailure(c *gin.Context, task *model.ImageTask, mt *meta.Meta, err error, failure failover.Failure, decision failover.Decision) {
	status, reason := adaptor.SubmissionEvidence(err)
	common.GetLogger(c).WithFields(logrus.Fields{
		"lane":            "image",
		"task_id":         task.ID,
		"model":           task.Model,
		"endpoint":        mt.ActualModel,
		"channel_id":      mt.Channel.ID,
		"attempt":         len(task.Attempts),
		"provider_status": status,
		"provider_reason": reason,
		"acceptance":      string(failure.Acceptance),
		"evidence":        failure.Evidence,
		"failover":        decision.Reason,
	}).Warn("image task provider submission failed")
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
