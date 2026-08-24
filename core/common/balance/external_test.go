package balance

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setFastExternalRetries(t *testing.T) {
	t.Helper()

	oldBalanceInterval := externalBalanceRetryInterval
	oldConsumeBackoff := externalConsumeBackoff

	externalBalanceRetryInterval = time.Millisecond
	externalConsumeBackoff = []time.Duration{
		time.Millisecond,
		time.Millisecond,
		time.Millisecond,
	}

	t.Cleanup(func() {
		externalBalanceRetryInterval = oldBalanceInterval
		externalConsumeBackoff = oldConsumeBackoff
	})
}

func writeExternalBalance(w http.ResponseWriter, balance float64) {
	fmt.Fprintf(w, `{"code":0,"message":"ok","data":{"balance":%v}}`, balance)
}

func writeExternalCharged(w http.ResponseWriter, charged float64) {
	fmt.Fprintf(w, `{"code":0,"message":"ok","data":{"charged":%v}}`, charged)
}

func TestExternalGetGroupRemainBalance(t *testing.T) {
	setFastExternalRetries(t)

	const group = "ext-test-happy"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, externalBalancePath, r.URL.Path)
		assert.Equal(t, group, r.URL.Query().Get("group"))
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		writeExternalBalance(w, 12.5)
	}))
	defer srv.Close()

	e := NewExternalHTTP(srv.URL, "test-key")

	remain, consumer, err := e.GetGroupRemainBalance(t.Context(), model.GroupCache{ID: group})
	require.NoError(t, err)
	require.InDelta(t, 12.5, remain, 0)
	require.NotNil(t, consumer)
}

func TestExternalBalanceCacheHit(t *testing.T) {
	setFastExternalRetries(t)

	const group = "ext-test-cache"

	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeExternalBalance(w, 5.5)
	}))
	defer srv.Close()

	e := NewExternalHTTP(srv.URL, "test-key")
	groupCache := model.GroupCache{ID: group, UsedAmount: 3}

	remain, _, err := e.GetGroupRemainBalance(t.Context(), groupCache)
	require.NoError(t, err)
	require.InDelta(t, 5.5, remain, 0)

	remain, _, err = e.GetGroupRemainBalance(t.Context(), groupCache)
	require.NoError(t, err)
	require.InDelta(t, 5.5, remain, 0)

	// GetGroupQuota shares the same cached fetch
	quota, err := e.GetGroupQuota(t.Context(), groupCache)
	require.NoError(t, err)
	require.InDelta(t, 8.5, quota.Total, 0)
	require.InDelta(t, 5.5, quota.Remain, 0)

	require.Equal(t, int64(1), hits.Load())
}

func TestExternalStaleServe(t *testing.T) {
	setFastExternalRetries(t)

	const group = "ext-test-stale"

	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			writeExternalBalance(w, 7)
			return
		}

		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := NewExternalHTTP(srv.URL, "test-key")

	remain, _, err := e.GetGroupRemainBalance(t.Context(), model.GroupCache{ID: group})
	require.NoError(t, err)
	require.InDelta(t, 7, remain, 0)

	// bypass the 1s local cache so the next call really hits the server
	externalBalanceCache.Delete(externalBalanceCacheKey(group))

	remain, consumer, err := e.GetGroupRemainBalance(t.Context(), model.GroupCache{ID: group})
	require.NoError(t, err)
	require.InDelta(t, 7, remain, 0)
	require.NotNil(t, consumer)
	require.Equal(t, int64(1+externalBalanceRetry), hits.Load())
}

func TestExternalConsumeRequestID(t *testing.T) {
	setFastExternalRetries(t)

	const group = "ext-test-request-id"

	reqCh := make(chan externalConsumeReq, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, externalConsumePath, r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		var req externalConsumeReq
		if err := sonic.ConfigDefault.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		reqCh <- req

		writeExternalCharged(w, 1.25)
	}))
	defer srv.Close()

	e := NewExternalHTTP(srv.URL, "test-key")
	consumer := newExternalPostGroupConsumer(e, group)

	ctx := context.WithValue(t.Context(), CtxRequestID, "req-abc123")
	ctx = ContextWithPricing(ctx, "USD", "21")

	charged, err := consumer.PostGroupConsume(ctx, "token-1", 1.25)
	require.NoError(t, err)
	require.InDelta(t, 1.25, charged, 0)

	gotReq := <-reqCh
	require.Equal(t, group, gotReq.Group)
	require.Equal(t, "token-1", gotReq.TokenName)
	require.InDelta(t, 1.25, gotReq.Amount, 0)
	require.Equal(t, "req-abc123", gotReq.RequestID)
	require.Equal(t, "USD", gotReq.Currency)
	require.Equal(t, "21", gotReq.PricingVersion)
}

func TestExternalConsumeRetry(t *testing.T) {
	setFastExternalRetries(t)

	const group = "ext-test-consume-retry"

	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		writeExternalCharged(w, 3.3)
	}))
	defer srv.Close()

	e := NewExternalHTTP(srv.URL, "test-key")
	consumer := newExternalPostGroupConsumer(e, group)

	charged, err := consumer.PostGroupConsume(t.Context(), "token-1", 3.3)
	require.NoError(t, err)
	require.InDelta(t, 3.3, charged, 0)
	require.Equal(t, int64(3), hits.Load())
}

func TestExternalConsumeZeroUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called for usage <= 0")
	}))
	defer srv.Close()

	consumer := newExternalPostGroupConsumer(NewExternalHTTP(srv.URL, "test-key"), "ext-test-zero")

	charged, err := consumer.PostGroupConsume(t.Context(), "token-1", 0)
	require.NoError(t, err)
	require.InDelta(t, 0, charged, 0)
}

func TestInitExternal(t *testing.T) {
	oldDefault := Default
	t.Cleanup(func() { Default = oldDefault })

	require.Error(t, InitExternal("", "key"))
	require.Error(t, InitExternal("http://example.com", ""))
	require.Error(t, InitExternal("", ""))
	require.Same(t, oldDefault, Default)

	require.NoError(t, InitExternal("http://example.com/", "key"))

	e, ok := Default.(*ExternalHTTP)
	require.True(t, ok)
	require.Equal(t, "http://example.com", e.url)
	require.Equal(t, "key", e.key)
}
