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
	log "github.com/sirupsen/logrus"
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
	// Log describes the request-log row recorded once the task is submitted.
	Log *model.NativeTaskLog
}
type Engine struct {
	DB       *gorm.DB
	Wallet   Wallet
	Provider Provider
}

var ErrUnavailable = errors.New("native execution unavailable")

// ErrNotNativeModel means the requested model has no native task route at all,
// which the caller can fix by choosing another model; ErrUnavailable is ours.
var ErrNotNativeModel = errors.New("model has no native task route")
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
		if errors.Is(err, balance.ErrPrepaymentInsufficientBalance) || errors.Is(err, balance.ErrPrepaymentTooManyActiveTasks) || errors.Is(err, balance.ErrPrepaymentModelPaused) {
			// The wallet refused before holding anything. Drop the reservation so a
			// refused request leaves no row behind; the same request may be retried.
			if _, releaseErr := model.ReleaseNativeReservation(e.DB, id, group, token, task.UpdatedAt); releaseErr != nil {
				log.Errorf("release refused native reservation %s: %v", id, releaseErr)
			}
		}
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
	if p.Log != nil {
		// The request log is an operator view; it never blocks a paid submission.
		if logErr := model.RecordNativeTaskLog(e.DB, task, *p.Log); logErr != nil {
			log.Errorf("record native task log %s: %v", id, logErr)
		}
	}
	upstreamID, submitErr := e.Provider.SubmitNative(ctx, task.Endpoint, []byte(task.NativeInput))
	if submitErr != nil {
		state, code := "submission_unknown", "submission_outcome_unknown"
		var issues []nativeresult.ParameterIssue
		var rejected *adaptor.ImageSubmissionFailure
		if errors.As(submitErr, &rejected) && rejected.Failure.Acceptance == failover.NotAccepted {
			// Persist the rejection before callbacks; recovery can replay both identities.
			state, code = "failed", "upstream_rejected"
			if adaptor.ProviderUnavailable(submitErr) {
				// Owner rule 2026-10-08: the provider created no request for a
				// reason that is not the customer's input (401/402/403/404/429,
				// or the connection failed before the request was sent).
				code = model.UpstreamUnavailableCode
			} else if issues = contract.CustomerIssues(rejectedIssues(rejected.PublicError)); len(issues) > 0 {
				// Owner decision 2026-10-09: name the rejected fields. Billing is
				// still the not-accepted refund of upstream_rejected. A rejected
				// platform control alone stays upstream_rejected.
				code = model.InvalidParametersCode
			}
		}
		logSubmissionFailure(task, submitErr, state, code)
		if err = model.TransitionNativeSubmission(e.DB, id, group, token, "submitting", state, code, issues...); err != nil {
			return task, err
		}
		task.Status = state
		task.ErrorCode = code
		task.PublicError = model.NativeTaskPublicError(code, issues)
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

// rejectedIssues are the fields an input rejection named, when the adaptor's
// public error is invalid_parameters with issues storage accepts; else none.
func rejectedIssues(public *model.ImageTaskError) []nativeresult.ParameterIssue {
	if public == nil || public.Code != model.InvalidParametersCode {
		return nil
	}
	issues := make([]nativeresult.ParameterIssue, 0, len(public.Issues))
	for _, issue := range public.Issues {
		issues = append(issues, nativeresult.ParameterIssue{Field: issue.Field, Rule: issue.Rule})
	}
	if !nativeresult.ValidParameterIssues(issues) {
		return nil
	}
	return issues
}

// notAccepted reports a failed submission the provider did not accept. Each
// refunds the hold in full as wallet reason not_accepted. invalid_parameters
// is one only without an upstream ID; with one it failed after acceptance.
func notAccepted(task *model.NativeTask) bool {
	if task.Status != "failed" {
		return false
	}
	switch task.ErrorCode {
	case "upstream_rejected", model.UpstreamUnavailableCode:
		return true
	case model.InvalidParametersCode:
		return task.UpstreamID == ""
	}
	return false
}

// failedAfterAcceptance reports a task the provider accepted and then failed
// or rejected. Each is refunded in full (execution_finished, then settle
// failed/platform_failure).
func failedAfterAcceptance(task *model.NativeTask) bool {
	if task.Status != "failed" {
		return false
	}
	switch task.ErrorCode {
	case "upstream_task_failed", "upstream_result_rejected":
		return true
	case model.InvalidParametersCode:
		return task.UpstreamID != ""
	}
	return false
}

// logSubmissionFailure records every failed provider submission for operators
// (owner rule 2026-10-08). It logs identities, the provider status and the
// adaptor's sanitized reason only: never credentials, request bodies or the
// customer's input.
func logSubmissionFailure(task *model.NativeTask, err error, state, code string) {
	status, reason := adaptor.SubmissionEvidence(err)
	acceptance := failover.FromError(err).Acceptance
	log.WithFields(log.Fields{
		"lane":            "native",
		"task_id":         task.ID,
		"group":           task.GroupID,
		"model":           task.Model,
		"endpoint":        task.Endpoint,
		"channel_id":      task.ChannelID,
		"provider_status": status,
		"provider_reason": reason,
		"acceptance":      string(acceptance),
		"task_status":     state,
		"error_code":      code,
	}).Warn("native task provider submission failed")
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
	if notAccepted(task) {
		if _, err := call(balance.PrepaymentCommand{Action: "reject_attempt", AttemptID: task.BillingOperationID + ":1"}); err != nil {
			return err
		}
		if _, err := call(balance.PrepaymentCommand{Action: "settle", Outcome: &balance.PrepaymentOutcome{Kind: "failed", Reason: "not_accepted"}}); err != nil {
			return err
		}
	}
	if failedAfterAcceptance(task) {
		if _, err := call(balance.PrepaymentCommand{Action: "execution_finished"}); err != nil {
			return err
		}
		// Owner decision 2026-10-02 (option A): the customer received nothing, so
		// refund in full. Any later provider bill is booked as platform loss.
		// fal bills nothing when it rejects the input after acceptance.
		if _, err := call(balance.PrepaymentCommand{Action: "settle", Outcome: &balance.PrepaymentOutcome{Kind: "failed", Reason: "platform_failure"}}); err != nil {
			return err
		}
	}
	if task.Status == "failed" && task.ErrorCode == model.AsyncGenerationTimeoutCode {
		// Owner rule 2026-10-08: refund in full in one wallet call, reason
		// platform_failure. execution_finished is not sent: between it and the
		// settle, a provider bill could be settled as an actual charge.
		if _, err := call(balance.PrepaymentCommand{Action: "settle", Outcome: &balance.PrepaymentOutcome{Kind: "failed", Reason: "platform_failure"}}); err != nil {
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
	task.PublicError = saved.PublicError
	return nil
}
