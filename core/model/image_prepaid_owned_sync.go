package model

import (
	"context"
	"maps"
	"sync/atomic"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// A process-private, one-run eligibility; no JSON/DB/request field can mint it.
// With no installed owner protocol it preserves legacy execution, not coverage.
type (
	imageSyncPermit         struct{ started atomic.Bool }
	imageWalletClaimContext struct {
		store   *ChannelAdmissionStore
		control *ChannelAdmissionControl
	}
)

func imageWalletPollTransaction(
	ctx context.Context,
	info *AsyncUsageInfo,
	work func(*gorm.DB, *imageWalletClaimContext) error,
) error {
	if info != nil && info.imagePoll != nil {
		return imagePollTransaction(ctx, info, work)
	}

	store, err := channelAdmissionRuntimeStore(LogDB)
	if err != nil {
		return err
	}

	if store == nil {
		return LogDB.WithContext(ctx).
			Transaction(func(tx *gorm.DB) error { return work(tx, nil) }, claimTxOptions(LogDB)...)
	}

	present, err := imageRecoveryClaimStoragePresent(LogDB)
	if err != nil {
		return err
	}

	if !present {
		return LogDB.WithContext(ctx).
			Transaction(func(tx *gorm.DB) error { return work(tx, nil) }, claimTxOptions(LogDB)...)
	}

	return store.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		present, err := imageRecoveryClaimStoragePresent(tx)
		if err != nil {
			return err
		}

		if !present {
			return ErrChannelAdmissionUnavailable
		}

		if _, err := imageRecoveryHandoffStoragePresent(tx); err != nil {
			return err
		}

		inactive, err := channelAdmissionNeverActivated(tx, c)
		if err != nil {
			return err
		}

		if inactive {
			return work(tx, nil)
		}

		return work(tx, &imageWalletClaimContext{store, c})
	}, true)
}

func prepareImageWalletSync(
	tx *gorm.DB,
	authority *imageWalletClaimContext,
	task *ImageTask,
	usage *AsyncUsageInfo,
	priority bool,
) (*ImageTask, error) {
	if task.PrepaymentQuoteJSON == "" || task.BillingSettled {
		return nil, nil
	}

	if task.BillingOperationID == "" || task.BillingOperationID != usage.BillingOperationID {
		return nil, ErrImagePollClaimLost
	}

	var fresh ImageTask
	if err := tx.First(&fresh, "id=?", task.ID).Error; err != nil {
		return nil, err
	}

	if priority && fresh.Status != "failed" {
		return nil, ErrImagePollClaimLost
	}

	if authority != nil {
		owner, err := newAdmissionGeneration()
		if err != nil {
			return nil, err
		}

		handle, won, err := claimImageRecoveryInTransaction(
			tx,
			authority.control,
			authority.store,
			&fresh,
			owner,
			priority,
		)
		if err != nil {
			return nil, err
		}

		if !won {
			return nil, nil
		}

		fresh.imageRecoveryClaim = handle
		fresh.AdmissionGeneration = handle.row.UseID
	}

	fresh.imageSyncPermit = &imageSyncPermit{}

	return &fresh, nil
}

func finishImageWalletClaimCheck(
	tx *gorm.DB,
	authority *imageWalletClaimContext,
	task *ImageTask,
) error {
	if task == nil || task.imageRecoveryClaim == nil {
		return nil
	}
	return checkImageRecoveryClaim(tx, authority.control, task, &task.imageRecoveryClaim.row, false)
}

func withImageWalletPollClaim(
	ctx context.Context,
	info *AsyncUsageInfo,
	observed *ImageTask,
	write func(*gorm.DB, *ImageTask, *AsyncUsageInfo, func() error) error,
) error {
	if info == nil || LogDB == nil {
		return ErrImagePollClaimLost
	}

	var granted *ImageTask

	err := imageWalletPollTransaction(
		ctx,
		info,
		func(tx *gorm.DB, authority *imageWalletClaimContext) error {
			err := withImagePollClaimTx(
				tx,
				info,
				observed,
				func(tx *gorm.DB, task *ImageTask, usage *AsyncUsageInfo) error {
					return write(tx, task, usage, func() error {
						var err error

						granted, err = prepareImageWalletSync(tx, authority, task, usage, true)
						return err
					})
				},
			)
			if err != nil {
				return err
			}

			return finishImageWalletClaimCheck(tx, authority, granted)
		},
	)
	if err == nil {
		info.imageTerminalWallet = granted
	}

	return err
}

// ClaimImagePrepaymentFromPoll proves the original pending poll owner in the
// same transaction as normal sidecar acquisition. A future schedule defers work.
func ClaimImagePrepaymentFromPoll(
	ctx context.Context,
	info *AsyncUsageInfo,
	task *ImageTask,
) (bool, error) {
	if task == nil || info == nil || LogDB == nil || task.imageRecoveryClaim != nil ||
		task.imageSyncPermit != nil {
		return false, ErrImagePollClaimLost
	}

	var granted *ImageTask

	err := imageWalletPollTransaction(
		ctx,
		info,
		func(tx *gorm.DB, authority *imageWalletClaimContext) error {
			err := withImagePollClaimTx(
				tx,
				info,
				task,
				func(tx *gorm.DB, current *ImageTask, usage *AsyncUsageInfo) error {
					if current.Status != task.Status {
						return ErrImagePollClaimLost
					}

					var err error

					granted, err = prepareImageWalletSync(tx, authority, current, usage, false)

					return err
				},
			)
			if err != nil {
				return err
			}

			return finishImageWalletClaimCheck(tx, authority, granted)
		},
	)
	if err != nil {
		return false, err
	}

	if granted == nil {
		return false, nil
	}

	*task = *granted

	return true, nil
}

// ImageTerminalWalletTask returns only this successful terminal transaction's
// private wallet capability. Copies share a one-use permit; no task-ID lookup.
func ImageTerminalWalletTask(info *AsyncUsageInfo, task *ImageTask) *ImageTask {
	if info == nil || task == nil {
		return nil
	}

	t := info.imageTerminalWallet
	if t == nil || t.ID != task.ID || t.GroupID != task.GroupID || t.TokenID != task.TokenID ||
		t.BillingOperationID != task.BillingOperationID {
		return nil
	}

	return t
}

func StartImagePrepaymentSync(task *ImageTask) error {
	if task == nil || task.imageSyncPermit == nil ||
		!task.imageSyncPermit.started.CompareAndSwap(false, true) {
		return ErrChannelAdmissionConflict
	}

	return nil
}

func ImagePrepaymentRecoveryOwned(task *ImageTask) bool {
	return task != nil && task.imageRecoveryClaim != nil
}

// CompleteClaimedPrepaidImageUsage consumes only an already durable wallet
// receipt. It never calls the wallet or edits sidecar/executor/use evidence.
//
//nolint:gocyclo // Completion checks owned claim and wallet receipt before writing both usage and log accounting in one transaction.
func CompleteClaimedPrepaidImageUsage(
	ctx context.Context,
	info *AsyncUsageInfo,
	observed *ImageTask,
) error {
	return withImagePollClaim(
		ctx,
		info,
		observed,
		func(tx *gorm.DB, task *ImageTask, current *AsyncUsageInfo) error {
			bill := task.Billing
			if task.Status != "completed" || task.PrepaymentQuoteJSON == "" ||
				!task.BillingSettled ||
				task.BillingOperationID == "" ||
				task.BillingOperationID != current.BillingOperationID ||
				current.MeasuredImage ||
				bill == nil ||
				bill.Currency != "USD" ||
				bill.ActualMicros == nil ||
				bill.RefundMicros == nil {
				return ErrImagePollClaimLost
			}

			charged, refund := *bill.ActualMicros, *bill.RefundMicros

			quote, err := ParseImagePrepaymentQuote(task.PrepaymentQuoteJSON)
			if err != nil || quote.PrepaidMicros != bill.PrepaidMicros ||
				(quote.SettlementPolicy == "actual-cost-v1" && bill.Status == "estimated") ||
				(quote.SettlementPolicy == "" && charged > bill.PrepaidMicros) {
				return ErrImagePollClaimLost
			}

			const maxSafe = 9007199254740991
			if bill.PrepaidMicros < 0 || bill.PrepaidMicros > maxSafe || charged < 0 ||
				charged > maxSafe ||
				refund < 0 ||
				refund > maxSafe ||
				(charged <= bill.PrepaidMicros && charged+refund != bill.PrepaidMicros) ||
				(charged > bill.PrepaidMicros && refund != 0) ||
				(bill.Status != "settled" && bill.Status != "estimated" && bill.Status != "refunded") ||
				(bill.Status == "refunded" && charged != 0) ||
				len(task.Data) == 0 {
				return ErrImagePollClaimLost
			}

			var entry Log

			if current.LogID <= 0 {
				return ErrImagePollClaimLost
			}

			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id=?", current.LogID).
				First(&entry).
				Error; err != nil {
				return err
			}

			if entry.GroupID != current.GroupID || entry.TokenID != current.TokenID ||
				(current.Mode != 0 && entry.Mode != current.Mode) ||
				string(entry.BillingOperationID) != current.BillingOperationID ||
				(entry.AsyncUsageStatus != AsyncUsageStatusPending && entry.AsyncUsageStatus != AsyncUsageStatusCompleted) {
				return ErrImagePollClaimLost
			}

			clock, err := asyncUsageLeaseClockSQL(tx)
			if err != nil {
				return err
			}

			var now int64
			if err = tx.Raw("SELECT " + clock).Scan(&now).Error; err != nil {
				return err
			}

			if now <= 0 || current.NextPollAt.UnixMilli() <= now {
				return ErrImagePollClaimLost
			}

			amount := float64(charged) / 1000000
			update := &AsyncUsageInfo{
				Usage:        Usage{ImageOutputTokens: ZeroNullInt64(len(task.Data))},
				UsageContext: current.UsageContext,
				Price:        Price{},
				Amount:       Amount{UsedAmount: amount, ImageOutputAmount: amount},
			}

			values, err := asyncUsageUpdateValues(
				update,
				"Usage",
				"UsageContext",
				"Price",
				"Amount",
			)
			if err != nil {
				return err
			}

			logValues := map[string]any{}
			maps.Copy(logValues, values)

			logValues["currency"], logValues["pricing_version"], logValues["async_usage_status"] = current.PricingCurrency, current.PricingVersion, AsyncUsageStatusCompleted
			if err = writeImageAccountingLog(tx, &entry, logValues); err != nil {
				return err
			}

			values["balance_consumed"], values["status"], values["processing_token"], values["error"] = true, AsyncUsageStatusCompleted, "", ""

			return writeImageAccountingUsage(tx, current, values)
		},
	)
}
