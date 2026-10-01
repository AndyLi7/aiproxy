// Package nativetask coordinates native task execution with durable wallet claims.
package nativetask

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"gorm.io/gorm"
)

type Wallet interface {
	Prepayment(context.Context, balance.PrepaymentCommand) (balance.PrepaymentReceipt, error)
}
type Provider interface {
	SubmitNative(context.Context, string, []byte) (string, error)
}
type Plan struct {
	KeyFingerprint                       string
	Contract                             json.RawMessage
	ChannelID                            int
	Endpoint, CredentialScope, QuoteJSON string
	DeliveryBase                         string
}
type Engine struct {
	DB       *gorm.DB
	Wallet   Wallet
	Provider Provider
}

var ErrUnavailable = errors.New("native execution unavailable")
var ErrPending = errors.New("native submission requires reconciliation")
var taskID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Submit receives a server-resolved plan. Credentials, routing and quote are
// never accepted from the public body. No provider call occurs without a unique
// wallet attempt claim. Unknown outcomes keep the same durable identity.
func (e *Engine) Submit(ctx context.Context, id, group string, token int, body []byte, p Plan) (*model.NativeTask, error) {
	if e == nil || e.DB == nil || e.Wallet == nil || e.Provider == nil || !taskID.MatchString(id) {
		return nil, ErrUnavailable
	}
	fingerprint, fingerprintErr := hex.DecodeString(p.KeyFingerprint)
	if fingerprintErr != nil || len(fingerprint) != 32 {
		return nil, ErrUnavailable
	}
	contract, err := nativeresult.CompileTaskContract(p.Contract)
	if err != nil {
		return nil, err
	}
	input, err := contract.ValidateRequest(body)
	if err != nil {
		return nil, err
	}
	input, err = contract.PrepareUpstreamInput(input)
	if err != nil {
		return nil, err
	}
	var frozen nativeresult.TaskContract
	if json.Unmarshal(p.Contract, &frozen) != nil {
		return nil, ErrUnavailable
	}
	quote, err := model.ParseImagePrepaymentQuote(p.QuoteJSON)
	if err != nil {
		return nil, err
	}
	route := quote.Route(p.ChannelID, p.Endpoint)
	// Image-based quantities must first be resolved to a proven request total by
	// the registry. Native outputs cannot be counted by pretending they are images.
	if route == nil || route.Provider != "fal" || route.CredentialScope != p.CredentialScope || (route.QuantityMetric != "request" && route.QuantityMetric != "bounded_request") {
		return nil, ErrUnavailable
	}
	digest := sha256.Sum256(body)
	task, _, err := model.ReserveNativeTask(e.DB, model.NativeTask{ID: id, GroupID: group, TokenID: token, Model: frozen.Model, Fingerprint: hex.EncodeToString(digest[:]), OutputSchema: string(frozen.OutputSchema), FrozenContract: string(p.Contract), NativeInput: string(input), ChannelID: p.ChannelID, Endpoint: p.Endpoint, CredentialScope: p.CredentialScope, PrepaymentQuoteJSON: p.QuoteJSON, BillingOperationID: "native:" + id, DeliveryBase: p.DeliveryBase, KeyFingerprint: p.KeyFingerprint})
	if err != nil {
		return nil, err
	}
	if task.Status != "reserved" {
		return task, e.SyncBilling(ctx, task)
	}
	call := func(command balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
		command.Group = group
		command.BillingOperationID = task.BillingOperationID
		return e.Wallet.Prepayment(ctx, command)
	}
	if _, err = call(balance.PrepaymentCommand{Action: "admit", TaskID: id, ModelID: task.Model, Currency: "USD", PrepaidMicros: &quote.PrepaidMicros, EstimatedMicros: &route.EstimatedMicros, QuoteJSON: p.QuoteJSON}); err != nil {
		return task, err
	}
	attemptID := task.BillingOperationID + ":1"
	receipt, err := call(balance.PrepaymentCommand{Action: "begin_attempt", RouteID: route.RouteID, AttemptID: attemptID})
	if err != nil {
		return task, err
	}
	if !receipt.Claimed {
		return task, ErrPending
	}
	if err = model.TransitionNativeSubmission(e.DB, id, group, token, "reserved", "submitting", ""); err != nil {
		return task, err
	}
	task.Status = "submitting"
	upstreamID, submitErr := e.Provider.SubmitNative(ctx, task.Endpoint, []byte(task.NativeInput))
	if submitErr != nil {
		state, code := "submission_unknown", "submission_outcome_unknown"
		var rejected *adaptor.ImageSubmissionFailure
		if errors.As(submitErr, &rejected) && rejected.Failure.Acceptance == failover.NotAccepted {
			// Persist the rejection before callbacks; recovery can replay both identities.
			state, code = "failed", "upstream_rejected"
		}
		if err = model.TransitionNativeSubmission(e.DB, id, group, token, "submitting", state, code); err != nil {
			return task, err
		}
		task.Status = state
		task.ErrorCode = code
		if state == "failed" {
			if _, err = call(balance.PrepaymentCommand{Action: "reject_attempt", AttemptID: attemptID}); err != nil {
				return task, err
			}
			_, err = call(balance.PrepaymentCommand{Action: "settle", Outcome: &balance.PrepaymentOutcome{Kind: "failed", Reason: "not_accepted"}})
			return task, err
		}
		return task, ErrPending
	}
	if err = model.AcceptNativeTask(e.DB, id, group, token, upstreamID); err != nil {
		return task, err
	}
	task.Status = "queued"
	task.UpstreamID = upstreamID
	_, err = call(balance.PrepaymentCommand{Action: "accept_attempt", AttemptID: attemptID, UpstreamTaskID: upstreamID})
	return task, err
}

// SyncBilling performs only idempotent wallet callbacks; it never calls a provider.
// Polling/recovery can retry it after a crash between task persistence and wallet
// acknowledgement, always using the same operation and attempt identities.
func (e *Engine) SyncBilling(ctx context.Context, task *model.NativeTask) error {
	if e == nil || e.DB == nil || e.Wallet == nil || task == nil || task.BillingOperationID == "" {
		return ErrUnavailable
	}
	call := func(command balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
		command.Group = task.GroupID
		command.BillingOperationID = task.BillingOperationID
		return e.Wallet.Prepayment(ctx, command)
	}
	if task.UpstreamID != "" {
		if _, err := call(balance.PrepaymentCommand{Action: "accept_attempt", AttemptID: task.BillingOperationID + ":1", UpstreamTaskID: task.UpstreamID}); err != nil {
			return err
		}
	}
	if task.Status == "failed" && task.ErrorCode == "upstream_rejected" {
		if _, err := call(balance.PrepaymentCommand{Action: "reject_attempt", AttemptID: task.BillingOperationID + ":1"}); err != nil {
			return err
		}
		if _, err := call(balance.PrepaymentCommand{Action: "settle", Outcome: &balance.PrepaymentOutcome{Kind: "failed", Reason: "not_accepted"}}); err != nil {
			return err
		}
	}
	if task.Status == "failed" && (task.ErrorCode == "upstream_task_failed" || task.ErrorCode == "upstream_result_rejected") {
		if _, err := call(balance.PrepaymentCommand{Action: "execution_finished"}); err != nil {
			return err
		}
	}
	if task.Status == "result_received" || task.Status == "delivery_ready" || task.Status == "completed" {
		if _, err := call(balance.PrepaymentCommand{Action: "execution_finished"}); err != nil {
			return err
		}
	}
	if task.Status == "delivery_ready" || task.Status == "completed" {
		if _, err := call(balance.PrepaymentCommand{Action: "delivered"}); err != nil {
			return err
		}
		if task.Status == "delivery_ready" {
			if err := model.CompleteNativeTask(e.DB, task.ID, task.GroupID, task.TokenID); err != nil {
				return err
			}
			task.Status = "completed"
		}
	}
	receipt, err := call(balance.PrepaymentCommand{Action: "get"})
	if err != nil {
		return err
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err = model.SaveNativeTaskBillingReceipt(e.DB, task.ID, task.GroupID, task.TokenID, string(raw)); err != nil {
		return err
	}
	if err = model.SaveNativeBillingTerminal(e.DB, task.ID, task.GroupID, task.TokenID, string(raw)); err != nil {
		return err
	}
	// Return the durable terminal state as well as its receipt. Private trial
	// evidence must not report a stale billing flag until the next HTTP poll.
	saved, err := model.GetNativeTask(e.DB, task.ID, task.GroupID, task.TokenID)
	if err != nil {
		return err
	}
	task.BillingReceiptJSON = saved.BillingReceiptJSON
	task.BillingSettled = saved.BillingSettled
	task.Status = saved.Status
	task.ErrorCode = saved.ErrorCode
	return nil
}
