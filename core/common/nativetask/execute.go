// Package nativetask coordinates native task execution with durable wallet claims.
package nativetask

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"

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
	// InputMeter is the model's raw input meter (model.InputMeterConfigKey),
	// or nil. It is decoded per request; an unusable one holds the maximum.
	InputMeter json.RawMessage
	// Log describes the request-log row recorded once the task is submitted.
	Log *model.NativeTaskLog
	// AcceptedModel is the request's model when the gateway resolved it to
	// this contract by another accepted ID (public_api_id or an alias); "" when
	// the request used the contract's own model. The task keeps the contract's
	// model either way.
	AcceptedModel string
}
type Engine struct {
	DB       *gorm.DB
	Wallet   Wallet
	Provider Provider
	// InputMeter sizes holds to the metered input. Off (env
	// DISABLE_NATIVE_INPUT_METER=true), every request holds the published maximum.
	InputMeter bool
	// AbandonReservations lets recovery fail and refund a reservation that was
	// never submitted (abandonReservation). Off (env
	// DISABLE_NATIVE_RESERVATION_STOP_LOSS=true), such a reservation waits for
	// the application's 24-hour fallback; rows already failed are still refunded.
	AbandonReservations bool
}

var ErrUnavailable = errors.New("native execution unavailable")

// ErrNotNativeModel means the requested model has no native task route at all,
// which the caller can fix by choosing another model; ErrUnavailable is ours.
var ErrNotNativeModel = errors.New("model has no native task route")
var ErrPending = errors.New("native submission requires reconciliation")
var taskID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// submitTimeout bounds one submission, detached from the client's connection
// (R3): a disconnect must not cancel a wallet call or the provider call
// midway, which would leave the hold or the provider outcome unknown. It
// covers the wallet budget below plus fal's 45-second submit.
const submitTimeout = 2 * time.Minute

// submitWalletBudget bounds admission and the attempt claim together,
// including their retries after a failed connection or an edge 5xx
// (balance.WithSubmitRetry, R1). Without retries both take at most 10
// seconds. The claim is sent only while the budget leaves it the full wallet
// request timeout (balance.FullRequestLeft). A variable so tests can shrink it.
var submitWalletBudget = 15 * time.Second

// Submit receives a server-resolved plan. Credentials, routing and quote are
// never accepted from the public body. No provider call occurs without a unique
// wallet attempt claim. Unknown outcomes keep the same durable identity.
func (e *Engine) Submit(ctx context.Context, id, group string, token int, body []byte, p Plan) (*model.NativeTask, error) {
	if e == nil || e.DB == nil || e.Wallet == nil || e.Provider == nil || !taskID.MatchString(id) {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), submitTimeout)
	defer cancel()
	if p.Log != nil {
		// The admission below may restamp the row's time and source; never
		// change the caller's plan.
		logInfo := *p.Log
		p.Log = &logInfo
	}
	fingerprint, fingerprintErr := hex.DecodeString(p.KeyFingerprint)
	if fingerprintErr != nil || len(fingerprint) != 32 {
		return nil, ErrUnavailable
	}
	contract, err := nativeresult.CompileTaskContract(p.Contract)
	if err != nil {
		return nil, err
	}
	input, err := contract.ValidateRequestAs(body, p.AcceptedModel)
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
	if e.InputMeter && len(p.InputMeter) > 0 {
		// Sized from the body sent upstream, before anything is reserved, so
		// the reservation, the wallet admission and every retry of the same
		// body share one quote.
		var meterErr error
		p.QuoteJSON, quote, meterErr = sizeQuoteToInput(p.QuoteJSON, quote, p.InputMeter, frozen, input)
		if meterErr != nil {
			log.WithFields(log.Fields{"lane": "native", "task_id": id, "group": group, "model": frozen.Model, "reason": meterErr.Error()}).
				Info("native input meter not applied; holding the published maximum")
		}
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
	walletCtx, cancelWallet := context.WithTimeout(balance.WithSubmitRetry(ctx), submitWalletBudget)
	defer cancelWallet()
	claim := func(command balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
		command.Group = group
		command.BillingOperationID = task.BillingOperationID
		return e.Wallet.Prepayment(walletCtx, command)
	}
	admit := balance.PrepaymentCommand{Action: "admit", TaskID: id, ModelID: task.Model, Currency: "USD", PrepaidMicros: &quote.PrepaidMicros, EstimatedMicros: &route.EstimatedMicros, QuoteJSON: p.QuoteJSON}
	if p.Log != nil {
		admit = admit.WithUsage(model.NativeUsageIdentity(task, *p.Log))
	}
	admitted, err := claim(admit)
	if err != nil {
		if WalletRefusal(err) {
			// The wallet refused before holding anything. Drop the reservation so a
			// refused request leaves no row behind; the same request may be retried.
			if _, releaseErr := model.ReleaseNativeReservation(e.DB, id, group, token, task.UpdatedAt); releaseErr != nil {
				log.Errorf("release refused native reservation %s: %v", id, releaseErr)
			}
		}
		if errors.Is(err, balance.ErrPrepaymentInsufficientBalance) {
			// The 402 names the hold this request asked for (the sized quote).
			err = &HoldRefusal{HoldMicros: quote.PrepaidMicros, Err: err}
		}
		if WalletRefusal(err) {
			return task, err
		}
		return e.storedUnlessReserved(task, err)
	}
	if p.Log != nil && admitted.UsageAtMs != nil {
		// The call row takes the hold's time and source: a retry of a reserved
		// task (an admit replay) stamps the first admission, so the row and
		// every money movement of the operation share one usage time. An
		// application without key usage P2 returns neither; the request start
		// is kept.
		p.Log.RequestAt = time.UnixMilli(*admitted.UsageAtMs).UTC()
		if model.ValidUsageSource(admitted.UsageSource) {
			p.Log.RequestSource = admitted.UsageSource
		}
	}
	attemptID := task.BillingOperationID + ":1"
	if !balance.FullRequestLeft(walletCtx) {
		// Admission used most of the budget (retries or a slow wallet). A
		// claim cut short could still be recorded after this request gave up,
		// leaving an attempt of unknown outcome (incident 2026-10-13). Send
		// none: the task stays reserved and its hold not_submitted, so a
		// same-ID retry replays the admission and claims; without one, the
		// stop-loss refunds the hold after reservationGrace.
		log.WithFields(log.Fields{"lane": "native", "task_id": id, "group": group, "model": task.Model}).
			Warn("native task wallet budget too short for the attempt claim; nothing claimed or submitted")
		return e.storedUnlessReserved(task, ErrPending)
	}
	receipt, err := claim(balance.PrepaymentCommand{Action: "begin_attempt", RouteID: route.RouteID, AttemptID: attemptID})
	if err != nil {
		return e.storedUnlessReserved(task, err)
	}
	if !receipt.Claimed {
		return e.storedUnlessReserved(task, ErrPending)
	}
	if err = model.TransitionNativeSubmission(e.DB, id, group, token, "reserved", "submitting", ""); err != nil {
		// Recovery may have failed the reservation (not_started) or another
		// request submitted it: answer that state, never a request ID conflict.
		return e.storedUnlessReserved(task, err)
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

// storedUnlessReserved returns the stored task with err when the task is no
// longer reserved, else task unchanged. A submission that failed before its
// provider call then reports what happened to the task meanwhile (recovery
// failed it as not_started, or another request submitted it), so the caller
// answers 202 with that state instead of a retry hint or a conflict.
func (e *Engine) storedUnlessReserved(task *model.NativeTask, err error) (*model.NativeTask, error) {
	if stored, readErr := model.GetNativeTask(e.DB, task.ID, task.GroupID, task.TokenID); readErr == nil && stored.Status != "reserved" {
		return stored, err
	}
	return task, err
}

// HoldRefusal is the wallet's insufficient-balance refusal of one request's
// hold. HoldMicros is the hold the admission asked for, after any input
// sizing. errors.Is still matches balance.ErrPrepaymentInsufficientBalance.
type HoldRefusal struct {
	HoldMicros int64
	Err        error
}

func (e *HoldRefusal) Error() string { return e.Err.Error() }
func (e *HoldRefusal) Unwrap() error { return e.Err }

// WalletRefusal reports the wallet's three explicit admission refusals. The
// wallet refuses atomically, so it holds nothing and the reservation is
// released; every other error after the reservation may leave a hold.
func WalletRefusal(err error) bool {
	return errors.Is(err, balance.ErrPrepaymentInsufficientBalance) ||
		errors.Is(err, balance.ErrPrepaymentTooManyActiveTasks) ||
		errors.Is(err, balance.ErrPrepaymentModelPaused)
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
// not_started was never sent to a provider at all.
func notAccepted(task *model.NativeTask) bool {
	if task.Status != "failed" {
		return false
	}
	switch task.ErrorCode {
	case "upstream_rejected", model.UpstreamUnavailableCode, model.NotStartedCode:
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

// refundNotStarted refunds a reservation recovery failed as not_started (owner
// decision 2026-10-13). The wallet may hold no attempt for it (the admission
// or the claim never landed), and reject_attempt on a missing attempt is
// refused for good, so the attempt is released only when the wallet lists it,
// best effort: it lets the application retire the row early. The settle
// failed/not_accepted is always sent, also when the list is unreadable or the
// release failed: it is valid from an unknown or not_submitted submission and
// refused only for an accepted one.
func refundNotStarted(task *model.NativeTask, call func(balance.PrepaymentCommand) (balance.PrepaymentReceipt, error)) error {
	attemptID := task.BillingOperationID + ":1"
	current, err := call(balance.PrepaymentCommand{Action: "get"})
	if err != nil {
		return err
	}
	fields := log.Fields{"lane": "native", "task_id": task.ID, "group": task.GroupID, "model": task.Model}
	if _, present, attemptsErr := current.AttemptState(attemptID); attemptsErr != nil {
		log.WithFields(fields).WithError(attemptsErr).Warn("native not_started refund: wallet attempts unreadable; settling without releasing the attempt")
	} else if present {
		if _, err := call(balance.PrepaymentCommand{Action: "reject_attempt", AttemptID: attemptID}); err != nil {
			log.WithFields(fields).WithError(err).Warn("native not_started refund: attempt not released; settling anyway")
		}
	}
	_, err = call(balance.PrepaymentCommand{Action: "settle", Outcome: &balance.PrepaymentOutcome{Kind: "failed", Reason: "not_accepted"}})
	return err
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
	if task.Status == "failed" && task.ErrorCode == model.NotStartedCode {
		if err := refundNotStarted(task, call); err != nil {
			return err
		}
	} else if notAccepted(task) {
		if _, err := call(balance.PrepaymentCommand{Action: "reject_attempt", AttemptID: task.BillingOperationID + ":1"}); err != nil {
			return err
		}
		if _, err := call(balance.PrepaymentCommand{Action: "settle", Outcome: &balance.PrepaymentOutcome{Kind: "failed", Reason: "not_accepted"}}); err != nil {
			return err
		}
	}
	if failedAfterAcceptance(task) {
		// Owner decision 2026-10-02 (option A): the customer received nothing, so
		// refund in full in one wallet call, reason platform_failure. Any later
		// provider bill is booked as platform loss. fal bills nothing when it
		// rejects the input after acceptance. As in the timeout branch below,
		// execution_finished is not sent: between it and the settle, a provider
		// bill could be settled as an actual charge.
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
