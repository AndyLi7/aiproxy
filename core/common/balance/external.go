package balance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/labring/aiproxy/core/model"
	gcache "github.com/patrickmn/go-cache"
	log "github.com/sirupsen/logrus"
)

const (
	externalBalancePath    = "/api/internal/wallet/balance"
	externalConsumePath    = "/api/internal/wallet/consume"
	externalRequestTimeout = 5 * time.Second
	externalBalanceRetry   = 3
	// 4 attempts = 3 retries, using the full 0.5s/1s/2s backoff ladder.
	externalConsumeRetry = 4
	externalStaleMaxAge  = 60 * time.Second
)

var (
	_ GroupBalance = (*ExternalHTTP)(nil)
	// The default transport keeps only 2 idle conns per host — far too few
	// for per-request balance checks at proxy QPS (TCP/TLS churn, port
	// exhaustion). Bodies are fully drained before close so conns are reused.
	externalHTTPClient = &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        64,
			MaxIdleConnsPerHost: 32,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	externalBalanceCache = gcache.New(time.Second, 5*time.Second)

	// retry intervals are variables so tests can shrink them
	externalBalanceRetryInterval = time.Second
	externalConsumeBackoff       = []time.Duration{
		500 * time.Millisecond,
		time.Second,
		2 * time.Second,
	}

	externalLastGoodMu sync.Mutex
	externalLastGood   = make(map[string]externalStaleBalance)
)

type externalStaleBalance struct {
	balance float64
	at      time.Time
}

type ctxKey struct{}

type pricingContextKey struct{}

type consumePricing struct {
	currency       string
	pricingVersion string
}

// CtxRequestID is the context key used to pass the request id to
// PostGroupConsume so the wallet backend can deduplicate retried charges.
var CtxRequestID ctxKey

// RequestIDFromContext returns the request id injected by the caller, or an
// empty string when none is present (the consume call then simply carries no
// idempotency key).
func RequestIDFromContext(ctx context.Context) string {
	if requestID, ok := ctx.Value(CtxRequestID).(string); ok {
		return requestID
	}
	return ""
}

// ContextWithPricing attaches the immutable retail-pricing provenance that
// the external wallet records alongside a debit.
func ContextWithPricing(ctx context.Context, currency, pricingVersion string) context.Context {
	return context.WithValue(ctx, pricingContextKey{}, consumePricing{
		currency:       strings.TrimSpace(currency),
		pricingVersion: strings.TrimSpace(pricingVersion),
	})
}

func pricingFromContext(ctx context.Context) consumePricing {
	pricing, _ := ctx.Value(pricingContextKey{}).(consumePricing)
	return pricing
}

// PricingFromContext returns pricing provenance previously attached with
// ContextWithPricing. Balance consumers can use it when bridging to an
// external ledger.
func PricingFromContext(ctx context.Context) (currency, pricingVersion string) {
	pricing := pricingFromContext(ctx)
	return pricing.currency, pricing.pricingVersion
}

// ExternalHTTP is a GroupBalance backend that talks to an external wallet
// service over HTTP (see docs/wallet-contract.md in the ShipAny repo).
type ExternalHTTP struct {
	url string
	key string
}

// InitExternal enables the external HTTP wallet backend as the default
// balance backend.
func InitExternal(rawURL, key string) error {
	rawURL = strings.TrimRight(rawURL, "/")
	if rawURL == "" {
		return errors.New("external balance url is empty")
	}

	if key == "" {
		return errors.New("external balance key is empty")
	}

	// A scheme-less/garbage URL must fail at startup, not turn every relay
	// request into a 500 at runtime.
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" {
		return fmt.Errorf("external balance url is invalid: %q", rawURL)
	}

	Default = NewExternalHTTP(rawURL, key)

	log.Info("external HTTP balance backend enabled: " + rawURL)

	return nil
}

func NewExternalHTTP(url, key string) *ExternalHTTP {
	return &ExternalHTTP{url: strings.TrimRight(url, "/"), key: key}
}

type externalBalanceResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Balance float64 `json:"balance"`
	} `json:"data"`
}

type externalConsumeReq struct {
	Group          string  `json:"group"`
	TokenName      string  `json:"tokenName"`
	Amount         float64 `json:"amount"`
	RequestID      string  `json:"requestId,omitempty"`
	Currency       string  `json:"currency,omitempty"`
	PricingVersion string  `json:"pricingVersion,omitempty"`
}

type externalConsumeResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Charged float64 `json:"charged"`
	} `json:"data"`
}

func externalBalanceCacheKey(group string) string {
	return "group:" + group
}

func externalSetLastGood(group string, balance float64) {
	externalLastGoodMu.Lock()
	defer externalLastGoodMu.Unlock()

	externalLastGood[group] = externalStaleBalance{balance: balance, at: time.Now()}
}

func externalGetLastGood(group string) (externalStaleBalance, bool) {
	externalLastGoodMu.Lock()
	defer externalLastGoodMu.Unlock()

	stale, ok := externalLastGood[group]

	return stale, ok
}

func (e *ExternalHTTP) GetGroupRemainBalance(
	ctx context.Context,
	group model.GroupCache,
) (float64, PostGroupConsumer, error) {
	remain, err := e.getRemainBalance(ctx, group.ID)
	if err != nil {
		return 0, nil, err
	}

	return remain, newExternalPostGroupConsumer(e, group.ID), nil
}

// GetGroupQuota implements GroupBalance.
func (e *ExternalHTTP) GetGroupQuota(
	ctx context.Context,
	group model.GroupCache,
) (*GroupQuota, error) {
	remain, err := e.getRemainBalance(ctx, group.ID)
	if err != nil {
		return nil, err
	}

	return &GroupQuota{
		Total:  remain + group.UsedAmount,
		Remain: remain,
	}, nil
}

func (e *ExternalHTTP) getRemainBalance(ctx context.Context, group string) (float64, error) {
	if v, ok := externalBalanceCache.Get(externalBalanceCacheKey(group)); ok {
		if balance, ok := v.(float64); ok {
			return balance, nil
		}
	}

	var lastErr error
	for i := range externalBalanceRetry {
		if i > 0 {
			time.Sleep(externalBalanceRetryInterval)
		}

		balance, err := e.fetchBalanceFromAPI(ctx, group)
		if err == nil {
			externalBalanceCache.Set(
				externalBalanceCacheKey(group),
				balance,
				gcache.DefaultExpiration,
			)
			externalSetLastGood(group, balance)

			return balance, nil
		}

		lastErr = err

		if ctx.Err() != nil {
			break
		}
	}

	// Never stale-serve a canceled context: the async poller treats a
	// pre-charge error as retryable, but a stale "success" during shutdown
	// would let the charge proceed into an ambiguous state.
	if ctx.Err() != nil {
		return 0, fmt.Errorf("get group (%s) balance canceled: %w", group, ctx.Err())
	}

	if stale, ok := externalGetLastGood(group); ok && time.Since(stale.at) <= externalStaleMaxAge {
		log.Warnf(
			"get group (%s) balance failed, serving stale balance from %s: %s",
			group,
			stale.at.Format(time.RFC3339),
			lastErr,
		)
		// Park the stale value in the 1s cache too, so an outage doesn't
		// hammer the wallet with full retry ladders on every request.
		externalBalanceCache.Set(
			externalBalanceCacheKey(group),
			stale.balance,
			gcache.DefaultExpiration,
		)

		return stale.balance, nil
	}

	return 0, fmt.Errorf("get group (%s) balance failed: %w", group, lastErr)
}

func (e *ExternalHTTP) fetchBalanceFromAPI(ctx context.Context, group string) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, externalRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		e.url+externalBalancePath+"?group="+url.QueryEscape(group),
		nil,
	)
	if err != nil {
		return 0, err
	}

	req.Header.Set("Authorization", "Bearer "+e.key)

	resp, err := externalHTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf(
			"get group (%s) balance failed with status code %d",
			group,
			resp.StatusCode,
		)
	}

	var balanceResp externalBalanceResp
	if err := sonic.ConfigDefault.NewDecoder(resp.Body).Decode(&balanceResp); err != nil {
		return 0, err
	}

	if balanceResp.Code != 0 {
		return 0, fmt.Errorf(
			"get group (%s) balance failed with code %d, message: %s",
			group,
			balanceResp.Code,
			balanceResp.Message,
		)
	}

	return balanceResp.Data.Balance, nil
}

type ExternalPostGroupConsumer struct {
	backend *ExternalHTTP
	group   string
}

func newExternalPostGroupConsumer(backend *ExternalHTTP, group string) *ExternalPostGroupConsumer {
	return &ExternalPostGroupConsumer{backend: backend, group: group}
}

func (c *ExternalPostGroupConsumer) PostGroupConsume(
	ctx context.Context,
	tokenName string,
	usage float64,
) (float64, error) {
	if usage <= 0 {
		return 0, nil
	}

	requestID := RequestIDFromContext(ctx)
	pricing := pricingFromContext(ctx)

	var lastErr error
	for i := range externalConsumeRetry {
		if i > 0 {
			time.Sleep(externalConsumeBackoff[i-1])
		}

		charged, err := c.backend.postConsume(
			ctx,
			c.group,
			tokenName,
			usage,
			requestID,
			pricing,
		)
		if err == nil {
			externalBalanceCache.Delete(externalBalanceCacheKey(c.group))
			return charged, nil
		}

		lastErr = err

		if ctx.Err() != nil {
			break
		}
	}

	return 0, fmt.Errorf("post group (%s) consume failed: %w", c.group, lastErr)
}

func (e *ExternalHTTP) postConsume(
	ctx context.Context,
	group, tokenName string,
	amount float64,
	requestID string,
	pricing consumePricing,
) (float64, error) {
	reqBody, err := sonic.Marshal(externalConsumeReq{
		Group:          group,
		TokenName:      tokenName,
		Amount:         amount,
		RequestID:      requestID,
		Currency:       pricing.currency,
		PricingVersion: pricing.pricingVersion,
	})
	if err != nil {
		return 0, err
	}

	ctx, cancel := context.WithTimeout(ctx, externalRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		e.url+externalConsumePath,
		bytes.NewReader(reqBody),
	)
	if err != nil {
		return 0, err
	}

	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := externalHTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf(
			"post group (%s) consume failed with status code %d",
			group,
			resp.StatusCode,
		)
	}

	var consumeResp externalConsumeResp
	if err := sonic.ConfigDefault.NewDecoder(resp.Body).Decode(&consumeResp); err != nil {
		return 0, err
	}

	if consumeResp.Code != 0 {
		return 0, fmt.Errorf(
			"post group (%s) consume failed with code %d, message: %s",
			group,
			consumeResp.Code,
			consumeResp.Message,
		)
	}

	return consumeResp.Data.Charged, nil
}
