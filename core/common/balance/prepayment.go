package balance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

var ErrPrepaymentInsufficientBalance = errors.New("insufficient prepayment balance")

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
}

func (e *ExternalHTTP) Prepayment(ctx context.Context, command PrepaymentCommand) (PrepaymentReceipt, error) {
	var receipt PrepaymentReceipt
	if command.Group == "" || command.BillingOperationID == "" {
		return receipt, errors.New("missing prepayment identity")
	}
	body, err := json.Marshal(command)
	if err != nil {
		return receipt, err
	}
	ctx, cancel := context.WithTimeout(ctx, externalRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/api/internal/wallet/prepayment", bytes.NewReader(body))
	if err != nil {
		return receipt, err
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	client := *externalHTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return receipt, errors.New("prepayment outcome unknown; retry original billing operation")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusPaymentRequired {
		return receipt, ErrPrepaymentInsufficientBalance
	}
	if response.StatusCode != http.StatusOK {
		return receipt, errors.New("prepayment unavailable")
	}
	var envelope struct {
		Code *int              `json:"code"`
		Data PrepaymentReceipt `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 128<<10)).Decode(&envelope) != nil || (envelope.Code == nil || *envelope.Code != 0) {
		return receipt, errors.New("prepayment rejected or unavailable")
	}
	if command.Action != "submitting" && envelope.Data.ID != command.BillingOperationID {
		return receipt, errors.New("prepayment receipt identity mismatch")
	}
	if command.Action != "submitting" && command.Action != "begin_attempt" {
		switch envelope.Data.Status {
		case "pending", "settled", "estimated", "refunded":
		default:
			return receipt, errors.New("invalid prepayment receipt state")
		}
		if envelope.Data.PrepaidMicros < 0 || envelope.Data.PrepaidMicros > 9007199254740991 {
			return receipt, errors.New("invalid prepayment receipt amount")
		}
		if envelope.Data.Status != "pending" {
			c, r := envelope.Data.ChargedMicros, envelope.Data.RefundMicros
			var policy struct {
				SettlementPolicy string `json:"settlementPolicy"`
			}
			if envelope.Data.QuoteJSON != "" && json.Unmarshal([]byte(envelope.Data.QuoteJSON), &policy) != nil {
				return receipt, errors.New("invalid receipt settlement policy")
			}
			if policy.SettlementPolicy != "" && policy.SettlementPolicy != "actual-cost-v1" {
				return receipt, errors.New("unsupported receipt settlement policy")
			}
			if c == nil || r == nil || *c < 0 || *c > 9007199254740991 || *r < 0 {
				return receipt, errors.New("invalid final prepayment receipt")
			}
			expectedRefund := envelope.Data.PrepaidMicros - *c
			if policy.SettlementPolicy == "actual-cost-v1" {
				if envelope.Data.Status == "estimated" {
					return receipt, errors.New("actual-cost receipt cannot be estimated")
				}
				if expectedRefund < 0 {
					expectedRefund = 0
				}
			} else if expectedRefund < 0 {
				return receipt, errors.New("legacy receipt exceeds prepayment")
			}
			if *r != expectedRefund {
				return receipt, errors.New("invalid final prepayment receipt")
			}
		}
	}
	return envelope.Data, nil
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
