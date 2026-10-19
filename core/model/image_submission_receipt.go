package model

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Private, process-local qualification from Prepare -> SaveAttempt. This is
// not an executor journal, a lease, or evidence of a producer's durable return.
// A crash loses qualification; reading a task never authorizes resubmission.
type imageSubmitReceipt struct {
	identity                                        ChannelIdentity
	index                                           int
	generation, attempt, facts, baseURL, usageFacts string
	createdAt                                       time.Time
	ready                                           bool
}

func imageSubmissionFacts(t *ImageTask) string {
	h, _ := admissionHash(
		[]any{
			t.ID,
			t.GroupID,
			t.TokenID,
			t.Model,
			t.RequestModel,
			t.Fingerprint,
			t.BillingOperationID,
			t.UsageID,
			t.ValidationContract,
			t.PrepaymentQuoteJSON,
			t.ChannelType,
			t.KeyFingerprint,
			t.UpstreamModel,
			t.ExpectedImages,
			t.ArchiveRequired,
			t.Attempts,
		},
	)

	return h
}

func imageSubmissionUsageFacts(u *AsyncUsageInfo) string {
	h, _ := admissionHash(
		[]any{
			u.ID,
			u.ImageTaskID,
			u.RequestID,
			u.GroupID,
			u.TokenID,
			u.BillingOperationID,
			u.LogID,
			u.Model,
			u.Price,
			u.PricingCurrency,
			u.PricingVersion,
			u.RequestAt,
			u.UsageContext,
			u.Usage,
			u.Amount,
			u.MeasuredImage,
			u.BalanceConsumed,
			u.BalanceConsumeAttempted,
			u.Mode,
			u.Capability,
			u.UsageModelID,
			u.UsageSource,
			u.UsageAtMs,
			u.DisableResolutionFuzzyMatch,
		},
	)

	return h
}

// No new deadline: this records an already-started action's result. It cannot
// authorize any external action. A winning timeout/terminal state always wins.
//
//nolint:gocyclo // Submission receipt guards retain task, attempt, usage, log-price, and producer closure checks around the write callback.
func withImageSubmissionReceipt(
	ctx context.Context,
	task *ImageTask,
	write func(*gorm.DB, *ImageTask) error,
	receiptKind string,
) error {
	if task == nil || task.imageSubmitReceipt == nil || !task.imageSubmitReceipt.ready ||
		ctx == nil {
		return ErrImageTaskConflict
	}

	actor := task.imageSubmitReceipt
	if actor.facts == "" || actor.usageFacts == "" {
		return ErrImageTaskConflict
	}

	if task.AdmissionGeneration != actor.generation || task.AdmissionAttempt != actor.attempt ||
		imageSubmissionFacts(task) != actor.facts {
		return ErrImageTaskConflict
	}

	store, err := channelAdmissionRuntimeStore(LogDB)
	if err != nil {
		return err
	}

	work := func(tx *gorm.DB, control *ChannelAdmissionControl) error {
		var current ImageTask
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&current, "id=? AND group_id=? AND token_id=?", task.ID, task.GroupID, task.TokenID).
			Error; e != nil {
			return e
		}

		if current.Status != "submitting" || current.UpstreamID != "" ||
			imageSubmissionFacts(&current) != actor.facts ||
			!imageAdmissionCreatedAtEqual(tx, current.CreatedAt, actor.createdAt) {
			return ErrImageTaskConflict
		}

		if control != nil {
			inactive, e := channelAdmissionNeverActivated(tx, control)
			if e != nil {
				return e
			}

			if !inactive {
				if _, e = lockAdmissionImageTask(tx, task); e != nil {
					return e
				}
				// Late installation never fabricates an attempt or adopts another use.
				if actor.attempt != "" {
					var use ChannelAdmissionUse
					if e = tx.First(&use, "id=?", actor.attempt).Error; e != nil {
						return e
					}

					if use.Lane != "image" || use.State != "open" ||
						use.ResourceKey != imageAttemptResource(task, actor.index) ||
						use.RequestHash != imageAttemptHash(task, actor.index, actor.identity) ||
						use.ChannelID != actor.identity.ChannelID ||
						use.KeyDigest != actor.identity.KeyDigest ||
						use.IdentityHash != actor.identity.IdentityHash {
						return ErrImageTaskConflict
					}

					channel, e := storedAdmissionIdentity(tx, actor.identity)
					if e != nil {
						return e
					}

					if channel.BaseURL != actor.baseURL {
						return ErrImageTaskConflict
					}
				} else if actor.generation != "" {
					return ErrImageTaskConflict
				}
			} else if actor.generation != "" || actor.attempt != "" {
				return ErrImageTaskConflict
			}
		} else if actor.generation != "" || actor.attempt != "" {
			return ErrImageTaskConflict
		}

		var usage AsyncUsageInfo
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&usage, "id=? AND image_task_id=?", current.UsageID, current.ID).
			Error; e != nil {
			return e
		}

		if imageSubmissionUsageFacts(&usage) != actor.usageFacts ||
			usage.Status != AsyncUsageStatusNone ||
			usage.ProcessingToken != "" ||
			usage.GroupID != current.GroupID ||
			usage.TokenID != current.TokenID ||
			usage.BillingOperationID != current.BillingOperationID ||
			usage.ChannelID != actor.identity.ChannelID ||
			usage.BaseURL != actor.baseURL {
			return ErrImageTaskConflict
		}

		if e := lockImageProducerReceipt(tx, control, task); e != nil {
			return e
		}

		var entry Log
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&entry, "id=? AND request_id=? AND group_id=? AND token_id=?", usage.LogID, current.ID, current.GroupID, current.TokenID).
			Error; e != nil {
			return e
		}

		logPrice, _ := admissionHash(entry.Price)

		usagePrice, _ := admissionHash(usage.Price)
		if logPrice == "" || logPrice != usagePrice ||
			entry.BillingOperationID.String() != current.BillingOperationID ||
			entry.ChannelID != usage.ChannelID ||
			entry.AsyncUsageStatus != AsyncUsageStatusPending {
			return ErrImageTaskConflict
		}

		if e := write(tx, &current); e != nil {
			return e
		}

		return imageProducerRecordReceipt(tx, control, task, receiptKind, "")
	}
	if store != nil {
		return store.transaction(ctx, work, true)
	}

	return LogDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return work(tx, nil) })
}

func AcceptSubmittedImageTask(ctx context.Context, task *ImageTask, upstream string) error {
	return withImageSubmissionReceipt(
		ctx,
		task,
		func(tx *gorm.DB, current *ImageTask) error { return acceptImageTask(tx, current.ID, upstream) },
		"queued",
	)
}

func CompleteSubmittedImageTask(
	ctx context.Context,
	task *ImageTask,
	data []ImageOutput,
	metadata ...ImageResultMetadata,
) error {
	return withImageSubmissionReceipt(ctx, task, func(tx *gorm.DB, current *ImageTask) error {
		return completeSyncImageTask(tx, current.ID, data, metadata...)
	}, "sync_result")
}

func SetSubmittedImageTaskResult(
	ctx context.Context,
	task *ImageTask,
	status string,
	taskError *ImageTaskError,
) error {
	if status != "failed" && status != "submission_unknown" {
		return ErrImageTaskConflict
	}

	return withImageSubmissionReceipt(ctx, task, func(tx *gorm.DB, current *ImageTask) error {
		if err := writeImageTaskResult(tx, current.ID, status, nil, taskError, false); err != nil {
			return err
		}

		if status != "failed" {
			return nil
		}

		var usage AsyncUsageInfo
		if err := tx.First(&usage, current.UsageID).Error; err != nil {
			return err
		}

		q := tx.Model(&AsyncUsageInfo{}).
			Where("id=? AND image_task_id=? AND status=?", usage.ID, current.ID, AsyncUsageStatusNone).
			Update("status", AsyncUsageStatusFailed)
		if q.Error != nil {
			return q.Error
		}

		if q.RowsAffected != 1 {
			return ErrImageTaskConflict
		}

		fields := map[string]any{
			"async_usage_status": AsyncUsageStatusFailed,
			"code":               502,
			"safe_error":         "Image generation failed",
		}
		if taskError != nil && PublicTaskErrorMessage(taskError.Code) != "" {
			fields["error_code"] = taskError.Code
		}

		q = tx.Model(&Log{}).
			Where("id=? AND request_id=? AND async_usage_status=?", usage.LogID, current.ID, AsyncUsageStatusPending).
			Updates(fields)
		if q.Error != nil {
			return q.Error
		}

		if q.RowsAffected != 1 {
			return ErrImageTaskConflict
		}

		return nil
	}, status)
}
