package balance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	externalConsumeRetry   = 3
	externalStaleMaxAge    = 60 * time.Second
)

var (
	_                    GroupBalance = (*ExternalHTTP)(nil)
	externalHTTPClient                = &http.Client{}
	externalBalanceCache              = gcache.New(time.Second, 5*time.Second)

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

// ExternalHTTP is a GroupBalance backend that talks to an external wallet
// service over HTTP (see docs/wallet-contract.md in the ShipAny repo).
type ExternalHTTP struct {
	url string
	key string
}

// InitExternal enables the external HTTP wallet backend as the default
// balance backend.
func InitExternal(url, key string) error {
	url = strings.TrimRight(url, "/")
	if url == "" {
		return errors.New("external balance url is empty")
	}

	if key == "" {
		return errors.New("external balance key is empty")
	}

	Default = NewExternalHTTP(url, key)

	log.Info("external HTTP balance backend enabled: " + url)

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
	Group     string  `json:"group"`
	TokenName string  `json:"tokenName"`
	Amount    float64 `json:"amount"`
	RequestID string  `json:"requestId,omitempty"`
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
	}

	if stale, ok := externalGetLastGood(group); ok && time.Since(stale.at) <= externalStaleMaxAge {
		log.Warnf(
			"get group (%s) balance failed, serving stale balance from %s: %s",
			group,
			stale.at.Format(time.RFC3339),
			lastErr,
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
	defer resp.Body.Close()

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

	var lastErr error
	for i := range externalConsumeRetry {
		if i > 0 {
			time.Sleep(externalConsumeBackoff[i-1])
		}

		charged, err := c.backend.postConsume(ctx, c.group, tokenName, usage, requestID)
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
) (float64, error) {
	reqBody, err := sonic.Marshal(externalConsumeReq{
		Group:     group,
		TokenName: tokenName,
		Amount:    amount,
		RequestID: requestID,
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
	defer resp.Body.Close()

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
