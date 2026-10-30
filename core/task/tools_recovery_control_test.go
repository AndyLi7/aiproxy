package task

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/stretchr/testify/require"
)

func TestToolsRecoveryStartupControl(t *testing.T) {
	for _, value := range []string{"", "false", "true"} {
		t.Run("disabled="+value, func(t *testing.T) {
			t.Setenv("DISABLE_UPSTREAM_BILLING_RECOVERY", value)
			var callbacks, imageRuns, polls, upstreamStarts atomic.Int32
			called := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/api/internal/wallet/reconcile", r.URL.Path)
				callbacks.Add(1)
				called <- struct{}{}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"code":0,"data":[]}`))
			}))
			defer server.Close()
			old := balance.Default
			balance.Default = balance.NewExternalHTTP(server.URL, "synthetic-only-recovery-key")
			defer func() { balance.Default = old }()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			imageStarted := make(chan struct{}, 1)
			polled := make(chan struct{}, 1)
			workers := configuredUsageRecoveryTasks(func(ctx context.Context) { imageRuns.Add(1); imageStarted <- struct{}{}; <-ctx.Done() }, func(ctx context.Context) {
				upstreamStarts.Add(1)
				ticks := make(chan time.Time, 1)
				ticks <- time.Now()
				runUpstreamBillingRecovery(ctx, ticks)
			})
			if value == "true" {
				require.Len(t, workers, 1, "disabled worker is not registered")
			} else {
				require.Len(t, workers, 2)
			}
			ticks := make(chan time.Time, 1)
			ticks <- time.Now()
			done := make(chan struct{})
			go func() {
				runAsyncUsagePollTask(ctx, ticks, func(context.Context) bool { polls.Add(1); polled <- struct{}{}; return false }, workers...)
				close(done)
			}()
			backgroundWait(t, imageStarted)
			backgroundWait(t, polled)
			if value != "true" {
				backgroundWait(t, called)
			}
			cancel()
			backgroundWait(t, done)
			require.EqualValues(t, 1, imageRuns.Load())
			require.EqualValues(t, 1, polls.Load())
			if value == "true" {
				require.Zero(t, upstreamStarts.Load())
				require.Zero(t, callbacks.Load())
			} else {
				require.EqualValues(t, 1, upstreamStarts.Load())
				require.EqualValues(t, 1, callbacks.Load())
			}
			require.NotNil(t, balance.Default, "artifact/wallet credentials remain configured")
		})
	}
}
