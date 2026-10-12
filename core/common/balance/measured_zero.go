package balance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/labring/aiproxy/core/model"
)

const externalMeasuredZeroPath = "/api/internal/wallet/measured-zero"

// MeasuredZeroRequest reports a measured image call whose complete bill is
// zero. The wallet writes one zero-amount receipt for it (key usage P2), which
// a customer read used to write.
type MeasuredZeroRequest struct {
	Group              string
	RequestID          string
	BillingOperationID string
	PricingVersion     string
	TokenName          string
	Usage              model.UsageIdentity
}

// MeasuredZeroReporter is implemented by wallets that accept measured-zero
// receipts. The async usage outbox calls it in place of a zero debit.
type MeasuredZeroReporter interface {
	MeasuredZero(ctx context.Context, request MeasuredZeroRequest) (string, error)
}

var _ MeasuredZeroReporter = (*ExternalHTTP)(nil)

type externalMeasuredZeroReq struct {
	Group              string `json:"group"`
	RequestID          string `json:"requestId"`
	BillingOperationID string `json:"billingOperationId,omitempty"`
	PricingVersion     string `json:"pricingVersion,omitempty"`
	TokenName          string `json:"tokenName,omitempty"`
	usageFields
}

// MeasuredZero posts the receipt request and returns the wallet's answer:
// created, existing or not_applicable. Any other answer is an error, so the
// outbox retries the same request (the receipt key is idempotent).
func (e *ExternalHTTP) MeasuredZero(ctx context.Context, request MeasuredZeroRequest) (string, error) {
	if request.Group == "" || request.RequestID == "" {
		return "", errors.New("missing measured-zero identity")
	}

	body, err := json.Marshal(externalMeasuredZeroReq{
		Group:              request.Group,
		RequestID:          request.RequestID,
		BillingOperationID: request.BillingOperationID,
		PricingVersion:     request.PricingVersion,
		TokenName:          request.TokenName,
		usageFields:        newUsageFields(request.Usage),
	})
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, externalRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		e.url+externalMeasuredZeroPath,
		bytes.NewReader(body),
	)
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")

	client := *externalHTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	response, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("measured-zero receipt unavailable: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("measured-zero receipt failed with status code %d", response.StatusCode)
	}

	var envelope struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Receipt string `json:"receipt"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&envelope); err != nil {
		return "", fmt.Errorf("decode measured-zero receipt: %w", err)
	}

	if envelope.Code == nil || *envelope.Code != 0 {
		return "", fmt.Errorf("measured-zero receipt rejected: %s", envelope.Message)
	}

	switch envelope.Data.Receipt {
	case "created", "existing", "not_applicable":
		return envelope.Data.Receipt, nil
	default:
		return "", fmt.Errorf("invalid measured-zero receipt %q", envelope.Data.Receipt)
	}
}
