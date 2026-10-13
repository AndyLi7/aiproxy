// Package imageprepayment bridges durable image attempts to the D34 wallet.
package imageprepayment

import (
	"context"
	"errors"
	"strconv"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/model"
)

func Call(ctx context.Context, task *model.ImageTask, command balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	client, ok := balance.Default.(*balance.ExternalHTTP)
	if !ok {
		return balance.PrepaymentReceipt{}, errors.New("external prepayment wallet unavailable")
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
			return balance.PrepaymentReceipt{}, err
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
		if err = model.SetImageTaskResult(task.ID, "failed", nil, &model.ImageTaskError{Code: "submission_timeout", Message: "Task could not be confirmed and was refunded"}); err != nil {
			return receipt, err
		}
		task.Status = "failed"
	}
	task.Billing = &model.ImageTaskBilling{Currency: "USD", Status: receipt.Status, PrepaidMicros: receipt.PrepaidMicros, ActualMicros: receipt.ChargedMicros, RefundMicros: receipt.RefundMicros}
	err = model.SaveImageTaskBilling(task)
	return receipt, err
}
