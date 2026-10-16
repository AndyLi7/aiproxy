// Package imageprepayment bridges durable image attempts to the D34 wallet.
package imageprepayment

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
)

// SubmitWalletBudget bounds the admission and the first attempt claim of one
// image submission together, including their retries after a failed
// connection or an edge 5xx (balance.WithSubmitRetry). Without retries both
// take at most 10 seconds. The claim is sent only while the budget leaves it
// the full wallet request timeout (balance.FullRequestLeft).
const SubmitWalletBudget = 15 * time.Second

// neverHeldAge is how long a failed task must have been failed before a
// wallet answer that it holds nothing ends its billing for good, like the
// native lane's stale reservations.
const neverHeldAge = time.Hour

// neverHeld reports a failed task whose admission never reached the wallet
// (R5): it claimed no attempt, failed more than neverHeldAge ago, and the
// wallet itself answered "Prepayment not found". A bare 404 never qualifies:
// it may come from the platform, not the wallet.
func neverHeld(task *model.ImageTask, err error, now time.Time) bool {
	return balance.WalletConfirmedNotFound(err) && len(task.Attempts) == 0 &&
		task.CompletedAt != nil && now.Sub(*task.CompletedAt) > neverHeldAge
}

func Call(ctx context.Context, task *model.ImageTask, command balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	client, ok := balance.Default.(*balance.ExternalHTTP)
	if !ok {
		return balance.PrepaymentReceipt{}, errors.New("external prepayment wallet unavailable")
	}
	if err := model.ValidateImagePrepaymentRecovery(ctx, task); err != nil {
		return balance.PrepaymentReceipt{}, err
	}
	command.Group = task.GroupID
	command.BillingOperationID = task.BillingOperationID
	return client.Prepayment(ctx, command)
}
func AttemptID(task *model.ImageTask, index int) string {
	return task.BillingOperationID + ":" + strconv.Itoa(index+1)
}

// Admit holds the task's prepayment. usage is the call's attribution (the
// reservation row's identity); the hold and every settlement row of the
// operation carry it. An incomplete identity sends none.
func Admit(ctx context.Context, task *model.ImageTask, channelID int, usage model.UsageIdentity) error {
	q, err := model.ParseImagePrepaymentQuote(task.PrepaymentQuoteJSON)
	if err != nil {
		return err
	}
	route := q.Route(channelID, task.UpstreamModel)
	if route == nil {
		return errors.New("unquoted image route")
	}
	command := balance.PrepaymentCommand{Action: "admit", TaskID: task.ID, ModelID: task.Model, Currency: "USD", PrepaidMicros: &q.PrepaidMicros, EstimatedMicros: &route.EstimatedMicros, QuoteJSON: task.PrepaymentQuoteJSON}
	_, err = Call(ctx, task, command.WithUsage(usage))
	return err
}
func Begin(ctx context.Context, task *model.ImageTask, channelID int, endpoint string, index int) (bool, error) {
	q, err := model.ParseImagePrepaymentQuote(task.PrepaymentQuoteJSON)
	if err != nil {
		return false, err
	}
	route := q.Route(channelID, endpoint)
	if route == nil {
		return false, errors.New("unquoted image route")
	}
	receipt, err := Call(ctx, task, balance.PrepaymentCommand{Action: "begin_attempt", RouteID: route.RouteID, AttemptID: AttemptID(task, index)})
	return receipt.Claimed, err
}
func Reject(ctx context.Context, task *model.ImageTask, index int) error {
	_, err := Call(ctx, task, balance.PrepaymentCommand{Action: "reject_attempt", AttemptID: AttemptID(task, index)})
	return err
}

// Sync never submits a provider request. All wallet callbacks replay one identity.
func Sync(ctx context.Context, task *model.ImageTask) (balance.PrepaymentReceipt, error) {
	if task.PrepaymentQuoteJSON == "" {
		return balance.PrepaymentReceipt{}, nil
	}
	if err := model.CaptureImageTaskGeneration(task); err != nil {
		return balance.PrepaymentReceipt{}, err
	}
	if len(task.Attempts) > 0 {
		last := task.Attempts[len(task.Attempts)-1]
		if last.Failure.Acceptance == failover.NotAccepted {
			if err := Reject(ctx, task, len(task.Attempts)-1); err != nil {
				return balance.PrepaymentReceipt{}, err
			}
		} else if task.UpstreamID != "" {
			if _, err := Call(ctx, task, balance.PrepaymentCommand{Action: "accept_attempt", AttemptID: AttemptID(task, len(task.Attempts)-1), UpstreamTaskID: task.UpstreamID}); err != nil {
				return balance.PrepaymentReceipt{}, err
			}
		}
	}
	if task.Status == "failed" {
		// Every failed task, whatever its error code, is refunded in full in
		// one wallet call, reason platform_failure. Owner rule 2026-10-02:
		// failed = not charged, and a provider bill that arrives later is
		// platform loss. Owner rule 2026-10-08: execution_finished is never
		// sent first, because between it and the settle a provider bill could
		// be settled as an actual charge.
		if _, err := Call(ctx, task, balance.PrepaymentCommand{Action: "settle", Outcome: &balance.PrepaymentOutcome{Kind: "failed", Reason: "platform_failure"}}); err != nil {
			if !neverHeld(task, err, time.Now()) {
				return balance.PrepaymentReceipt{}, err
			}
			// Nothing was held, so nothing is owed or refunded: close billing
			// instead of re-sending the settle every recovery pass forever.
			log.WithFields(log.Fields{"lane": "image", "task_id": task.ID, "group": task.GroupID}).
				Warn("image task failed without a wallet hold; billing closed with nothing to settle")
			zero := int64(0)
			task.Billing = &model.ImageTaskBilling{Currency: "USD", Status: "refunded", ActualMicros: &zero, RefundMicros: &zero}
			receipt := balance.PrepaymentReceipt{ID: task.BillingOperationID, Status: "refunded", ChargedMicros: &zero, RefundMicros: &zero}
			return receipt, model.SaveImageTaskBilling(task)
		}
	} else if task.Status == "completed" {
		if _, err := Call(ctx, task, balance.PrepaymentCommand{Action: "delivered"}); err != nil {
			return balance.PrepaymentReceipt{}, err
		}
	}
	receipt, err := Call(ctx, task, balance.PrepaymentCommand{Action: "get"})
	if err != nil {
		return receipt, err
	}
	if receipt.Status == "refunded" && task.Status != "failed" && task.Status != "completed" {
		if err = model.SetImageTaskResultGeneration(task, "failed", nil, &model.ImageTaskError{Code: "submission_timeout", Message: "Task could not be confirmed and was refunded"}); err != nil {
			return receipt, err
		}
		task.Status = "failed"
	}
	task.Billing = &model.ImageTaskBilling{Currency: "USD", Status: receipt.Status, PrepaidMicros: receipt.PrepaidMicros, ActualMicros: receipt.ChargedMicros, RefundMicros: receipt.RefundMicros}
	err = model.SaveImageTaskBilling(task)
	return receipt, err
}
