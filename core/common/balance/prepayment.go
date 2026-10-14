package balance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/labring/aiproxy/core/model"
)

var ErrPrepaymentInsufficientBalance = errors.New("insufficient prepayment balance")

// The wallet refuses these before any provider call, so the same request can
// be retried later without a double charge.
var (
	ErrPrepaymentTooManyActiveTasks = errors.New("too many active prepaid tasks")
	ErrPrepaymentModelPaused        = errors.New("model paused pending billing review")
)

// ErrPrepaymentNotFound means the wallet holds nothing for this operation.
var ErrPrepaymentNotFound = errors.New("prepayment not found")

// appNotFoundError is ErrPrepaymentNotFound in the application's own envelope
// ("Prepayment not found"): the wallet itself answered that it holds nothing.
type appNotFoundError struct{}

func (appNotFoundError) Error() string        { return ErrPrepaymentNotFound.Error() }
func (appNotFoundError) Is(target error) bool { return target == ErrPrepaymentNotFound }

// WalletConfirmedNotFound reports a 404 that carries the application's own
// "Prepayment not found" envelope. Any 404 still matches ErrPrepaymentNotFound;
// only this one may end billing for good, because a bare 404 (for example a
// platform error page during a deploy) proves nothing about the wallet.
func WalletConfirmedNotFound(err error) bool {
	return errors.Is(err, appNotFoundError{})
}

// Submission retries (R1, 2026-10-13): see WithSubmitRetry.
const prepaymentSubmitTries = 3

// prepaymentRetryBackoff is the wait before each retry; a variable so tests
// can shrink it.
var prepaymentRetryBackoff = []time.Duration{500 * time.Millisecond, time.Second}

type submitRetryKey struct{}

// WithSubmitRetry marks the wallet calls a submission makes before any
// provider call. Their admit and begin_attempt commands are sent up to
// prepaymentSubmitTries times, with the same body, after the connection could
// not be made or after a 5xx that did not come from the application (no app
// envelope; a 504 is excluded as a timeout). Such a 5xx does not prove the
// wallet never received the command: the platform edge can answer 502 after
// the application committed it, for example when a container is stopped
// mid-request. Retrying is safe only because both commands replay
// idempotently: a replayed admit returns the existing hold, and a replayed
// begin_attempt answers claimed:false, which never authorizes a provider
// call. Any action added to the retry set must replay the same way. A
// timeout, a refusal (402/423/429/404) or any application answer, including
// code -1, is never retried. The caller bounds all tries with its context
// deadline, and a retry is sent only while that deadline leaves it the full
// request timeout (FullRequestLeft). Recovery never uses it: it retries on
// its next pass.
func WithSubmitRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, submitRetryKey{}, true)
}

// FullRequestLeft reports whether ctx leaves one wallet command its whole
// request timeout. A command the caller's deadline cuts short may still be
// processed by the wallet after the caller gave up: a begin_attempt processed
// that way leaves a claimed attempt whose outcome the gateway never learns
// (incident 2026-10-13). A submission sends its attempt claim, and any retry,
// only while this holds.
func FullRequestLeft(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) >= externalRequestTimeout
}

func submitRetry(ctx context.Context, action string) bool {
	marked, _ := ctx.Value(submitRetryKey{}).(bool)
	return marked && (action == "admit" || action == "begin_attempt")
}

// neverDelivered reports a transport error raised before any byte of the
// request was sent: dialing failed (refused, unreachable, DNS).
func neverDelivered(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// appEnvelope reports a JSON body in the application's response envelope (a
// numeric code), and its message.
func appEnvelope(body []byte) (string, bool) {
	var envelope struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Code == nil {
		return "", false
	}
	return envelope.Message, true
}

// Prepayment callbacks are opt-in and never use the legacy balance cache.
// The durable caller retries the same operation ID after an ambiguous response.
type PrepaymentCommand struct {
	RouteID            string             `json:"routeId,omitempty"`
	AttemptID          string             `json:"attemptId,omitempty"`
	UpstreamTaskID     string             `json:"upstreamTaskId,omitempty"`
	Action             string             `json:"action"`
	Group              string             `json:"group"`
	BillingOperationID string             `json:"billingOperationId"`
	TaskID             string             `json:"taskId,omitempty"`
	ModelID            string             `json:"modelId,omitempty"`
	Currency           string             `json:"currency,omitempty"`
	PrepaidMicros      *int64             `json:"prepaidMicros,omitempty"`
	EstimatedMicros    *int64             `json:"estimatedMicros,omitempty"`
	QuoteJSON          string             `json:"quoteJson,omitempty"`
	Outcome            *PrepaymentOutcome `json:"outcome,omitempty"`
	// Key usage P2 attribution, sent on admit only (WithUsage). The
	// application must accept these keys before a gateway sends them: its
	// admit schema is strict.
	APIKeyID     *int   `json:"apiKeyId,omitempty"`
	UsageModelID string `json:"usageModelId,omitempty"`
	Source       string `json:"source,omitempty"`
	UsageAtMs    *int64 `json:"usageAtMs,omitempty"`
}

// WithUsage sets the call's attribution on an admit command when it is
// complete; an incomplete identity leaves the command unchanged. ModelID keeps
// its own meaning (the registry entry or contract id the hold is for).
func (c PrepaymentCommand) WithUsage(identity model.UsageIdentity) PrepaymentCommand {
	if !identity.Complete() {
		return c
	}

	fields := newUsageFields(identity)
	c.APIKeyID = fields.APIKeyID
	c.UsageModelID = fields.UsageModelID
	c.Source = fields.Source
	c.UsageAtMs = fields.UsageAtMs

	return c
}

type PrepaymentOutcome struct {
	Kind           string `json:"kind"`
	CustomerMicros *int64 `json:"customerMicros,omitempty"`
	EvidenceID     string `json:"evidenceId,omitempty"`
	Reason         string `json:"reason,omitempty"`
}
type PrepaymentReceipt struct {
	QuoteJSON          string `json:"quoteJson,omitempty"`
	PrepaidMicros      int64  `json:"prepaidMicros"`
	ID                 string `json:"id"`
	Claimed            bool   `json:"claimed"`
	Status             string `json:"status"`
	ChargedMicros      *int64 `json:"chargedMicros"`
	RefundMicros       *int64 `json:"refundMicros"`
	PlatformLossMicros *int64 `json:"platformLossMicros"`
	// The hold's usage time and source (key usage P2), read by the wallet from
	// the hold row. Absent for holds admitted without attribution and from an
	// application without P2; a value <= 0 or an unknown source is dropped.
	UsageAtMs   *int64 `json:"usageAtMs,omitempty"`
	UsageSource string `json:"usageSource,omitempty"`
	// The hold's submission state, read only by native reservation recovery.
	// Each is kept as the wallet sent it and parsed on use, where a value of
	// another type reads as unknown: a change to one of them never fails a
	// receipt, so no other wallet command depends on them. attemptsJson is a
	// JSON text column today; an array is read as well.
	SubmissionStatus json.RawMessage `json:"submissionStatus,omitempty"`
	AttemptsJSON     json.RawMessage `json:"attemptsJson,omitempty"`
	CreatedAt        json.RawMessage `json:"createdAt,omitempty"`
	UnknownSince     json.RawMessage `json:"unknownSince,omitempty"`
}

// ErrUnreadableAttempts means a receipt's attempt list is missing or not in a
// shape the gateway reads.
var ErrUnreadableAttempts = errors.New("prepayment attempts unreadable")

func receiptText(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

// Submission is the hold's submission status ("not_submitted", "unknown" or
// "accepted"), or "" when it is absent or not a string.
func (r PrepaymentReceipt) Submission() string {
	return receiptText(r.SubmissionStatus)
}

func receiptTime(raw json.RawMessage) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339Nano, receiptText(raw))
	return at, err == nil
}

// AdmittedAt is when the wallet admitted the hold; false when unreadable.
func (r PrepaymentReceipt) AdmittedAt() (time.Time, bool) {
	return receiptTime(r.CreatedAt)
}

// UnknownAt is when the hold's current attempt was claimed (its submission
// became unknown); false when absent or unreadable.
func (r PrepaymentReceipt) UnknownAt() (time.Time, bool) {
	return receiptTime(r.UnknownSince)
}

// AttemptState reports the state of one attempt in the hold's attempt list.
// present is false when the list does not name it; err is
// ErrUnreadableAttempts when the list is missing or malformed (never guessed).
func (r PrepaymentReceipt) AttemptState(id string) (state string, present bool, err error) {
	raw := []byte(r.AttemptsJSON)
	if text := receiptText(r.AttemptsJSON); text != "" {
		raw = []byte(text)
	}
	var attempts []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &attempts) != nil || attempts == nil {
		return "", false, ErrUnreadableAttempts
	}
	for _, attempt := range attempts {
		if attempt.ID == id {
			return attempt.State, true, nil
		}
	}
	return "", false, nil
}

// normalizeUsage drops a receipt's attribution unless both fields are usable,
// so callers either have the hold's time and source or neither.
func (r *PrepaymentReceipt) normalizeUsage() {
	if r.UsageAtMs == nil || *r.UsageAtMs <= 0 || !model.ValidUsageSource(r.UsageSource) {
		r.UsageAtMs = nil
		r.UsageSource = ""
	}
}

func (e *ExternalHTTP) Prepayment(ctx context.Context, command PrepaymentCommand) (PrepaymentReceipt, error) {
	if command.Group == "" || command.BillingOperationID == "" {
		return PrepaymentReceipt{}, errors.New("missing prepayment identity")
	}
	body, err := json.Marshal(command)
	if err != nil {
		return PrepaymentReceipt{}, err
	}
	tries := 1
	if submitRetry(ctx, command.Action) {
		tries = prepaymentSubmitTries
	}
	for try := 1; ; try++ {
		receipt, retryable, err := e.prepaymentOnce(ctx, command, body)
		if err == nil || !retryable || try >= tries {
			return receipt, err
		}
		select {
		case <-ctx.Done():
			return receipt, err
		case <-time.After(prepaymentRetryBackoff[try-1]):
		}
		if !FullRequestLeft(ctx) {
			// A retry the deadline would cut short could be processed after
			// this submission gave up; the same-ID retry replays it instead.
			return receipt, err
		}
	}
}

// prepaymentOnce sends one wallet command. retryable reports an error after
// which a marked submission command may be sent again (see WithSubmitRetry).
func (e *ExternalHTTP) prepaymentOnce(ctx context.Context, command PrepaymentCommand, body []byte) (receipt PrepaymentReceipt, retryable bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, externalRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/api/internal/wallet/prepayment", bytes.NewReader(body))
	if err != nil {
		return receipt, false, err
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	client := *externalHTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return receipt, neverDelivered(err), errors.New("prepayment outcome unknown; retry original billing operation")
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusPaymentRequired:
		return receipt, false, ErrPrepaymentInsufficientBalance
	case http.StatusTooManyRequests:
		return receipt, false, ErrPrepaymentTooManyActiveTasks
	case http.StatusLocked:
		return receipt, false, ErrPrepaymentModelPaused
	case http.StatusNotFound:
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 128<<10))
		if message, ok := appEnvelope(raw); ok && message == "Prepayment not found" {
			return receipt, false, appNotFoundError{}
		}
		return receipt, false, ErrPrepaymentNotFound
	}
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 128<<10))
		_, fromApp := appEnvelope(raw)
		// A 5xx page from the platform edge, not the application. The
		// application may still have processed the command (a container
		// stopped mid-request), so it is retryable only because the retried
		// commands replay idempotently (WithSubmitRetry).
		edge := response.StatusCode >= 500 && response.StatusCode != http.StatusGatewayTimeout && !fromApp
		return receipt, edge, errors.New("prepayment unavailable")
	}
	var envelope struct {
		Code *int              `json:"code"`
		Data PrepaymentReceipt `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 128<<10)).Decode(&envelope) != nil || (envelope.Code == nil || *envelope.Code != 0) {
		return receipt, false, errors.New("prepayment rejected or unavailable")
	}
	if command.Action != "submitting" && envelope.Data.ID != command.BillingOperationID {
		return receipt, false, errors.New("prepayment receipt identity mismatch")
	}
	if command.Action != "submitting" && command.Action != "begin_attempt" {
		switch envelope.Data.Status {
		case "pending", "settled", "estimated", "refunded":
		default:
			return receipt, false, errors.New("invalid prepayment receipt state")
		}
		if envelope.Data.PrepaidMicros < 0 || envelope.Data.PrepaidMicros > 9007199254740991 {
			return receipt, false, errors.New("invalid prepayment receipt amount")
		}
		if envelope.Data.Status != "pending" {
			c, r := envelope.Data.ChargedMicros, envelope.Data.RefundMicros
			var policy struct {
				SettlementPolicy string `json:"settlementPolicy"`
			}
			if envelope.Data.QuoteJSON != "" && json.Unmarshal([]byte(envelope.Data.QuoteJSON), &policy) != nil {
				return receipt, false, errors.New("invalid receipt settlement policy")
			}
			if policy.SettlementPolicy != "" && policy.SettlementPolicy != "actual-cost-v1" {
				return receipt, false, errors.New("unsupported receipt settlement policy")
			}
			if c == nil || r == nil || *c < 0 || *c > 9007199254740991 || *r < 0 {
				return receipt, false, errors.New("invalid final prepayment receipt")
			}
			expectedRefund := envelope.Data.PrepaidMicros - *c
			if policy.SettlementPolicy == "actual-cost-v1" {
				if envelope.Data.Status == "estimated" {
					return receipt, false, errors.New("actual-cost receipt cannot be estimated")
				}
				if expectedRefund < 0 {
					expectedRefund = 0
				}
			} else if expectedRefund < 0 {
				return receipt, false, errors.New("legacy receipt exceeds prepayment")
			}
			if *r != expectedRefund {
				return receipt, false, errors.New("invalid final prepayment receipt")
			}
		}
	}
	envelope.Data.normalizeUsage()

	return envelope.Data, false, nil
}

// RecoverPrepayments asks the application to process its durable billing queue.
func (e *ExternalHTTP) RecoverPrepayments(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/api/internal/wallet/reconcile", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	client := *externalHTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return errors.New("billing recovery unavailable")
	}
	defer response.Body.Close()
	var envelope struct {
		Code *int `json:"code"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 128<<10)).Decode(&envelope) != nil || envelope.Code == nil || *envelope.Code != 0 {
		return errors.New("billing recovery unavailable")
	}
	return nil
}
