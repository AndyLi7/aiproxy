package nativetask

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"testing"
)

type pollStub struct {
	result nativeresult.PollResult
	err    error
	calls  int
}

func (p *pollStub) PollNative(_ context.Context, endpoint, id string, c *nativeresult.CompiledTask) (nativeresult.PollResult, error) {
	p.calls++
	return p.result, p.err
}
func TestNativePollRunsAcceptedTaskToDelivered(t *testing.T) {
	e, plan, w, p := setup(t)
	ctx := context.Background()
	task, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	poll := &pollStub{result: nativeresult.PollResult{Status: "running"}}
	resolve := func(_ context.Context, recorded *model.NativeTask) (Poller, error) {
		require.Equal(t, task.Endpoint, recorded.Endpoint)
		require.Equal(t, "scope", recorded.CredentialScope)
		require.Equal(t, "upstream-1", recorded.UpstreamID)
		return poll, nil
	}
	running, err := e.Poll(ctx, "req", "g", 1, resolve, nil)
	require.NoError(t, err)
	require.Equal(t, "running", running.Status)
	poll.result.Status = "queued"
	running, err = e.Poll(ctx, "req", "g", 1, resolve, nil)
	require.NoError(t, err)
	require.Equal(t, "running", running.Status)
	poll.result = nativeresult.PollResult{Status: "result_received", Output: json.RawMessage(`{"seed":9007199254740993,"labels":[]}`)}
	done, err := e.Poll(ctx, "req", "g", 1, resolve, nil)
	require.NoError(t, err)
	require.Equal(t, "completed", done.Status)
	require.Contains(t, done.DeliveredOutput, "9007199254740993")
	_, err = e.Poll(ctx, "req", "g", 1, resolve, nil)
	require.NoError(t, err)
	require.Equal(t, 3, poll.calls)
	require.Equal(t, 1, p.calls)
	delivered := false
	for _, command := range w.commands {
		if command.Action == "delivered" {
			delivered = true
		}
	}
	require.True(t, delivered)
}
func TestNativePollFailureAndCredentialIsolation(t *testing.T) {
	e, plan, w, _ := setup(t)
	ctx := context.Background()
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	calls := 0
	resolve := func(context.Context, *model.NativeTask) (Poller, error) {
		calls++
		return nil, errors.New("credential changed")
	}
	_, err = e.Poll(ctx, "req", "other", 1, resolve, nil)
	require.Error(t, err)
	require.Zero(t, calls)
	_, err = e.Poll(ctx, "req", "g", 1, resolve, nil)
	require.Error(t, err)
	require.Equal(t, 1, calls)
	poll := &pollStub{result: nativeresult.PollResult{Status: "failed", ErrorCode: "upstream_task_failed"}}
	failed, err := e.Poll(ctx, "req", "g", 1, func(context.Context, *model.NativeTask) (Poller, error) { return poll, nil }, nil)
	require.NoError(t, err)
	require.Equal(t, "failed", failed.Status)
	require.Empty(t, failed.DeliveredOutput)
	refunded := false
	for _, c := range w.commands {
		if c.Action == "settle" && c.Outcome.Kind == "failed" {
			require.Equal(t, "platform_failure", c.Outcome.Reason)
			refunded = true
		}
	}
	// Owner decision 2026-10-02 (option A): no image, no charge.
	require.True(t, refunded, "an accepted provider failure must refund the customer")
}

func refundedWith(w *walletStub, reason string) bool {
	for _, c := range w.commands {
		if c.Action == "settle" && c.Outcome != nil && c.Outcome.Kind == "failed" && c.Outcome.Reason == reason {
			return true
		}
	}
	return false
}

func TestNativePermanentResultErrorsFailAndRefund(t *testing.T) {
	e, plan, w, _ := setup(t)
	ctx := context.Background()
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	poll := &pollStub{err: nativeresult.ErrTooLarge}
	resolve := func(context.Context, *model.NativeTask) (Poller, error) { return poll, nil }
	task, err := e.Poll(ctx, "req", "g", 1, resolve, nil)
	require.NoError(t, err)
	require.Equal(t, "failed", task.Status)
	require.Equal(t, "upstream_result_rejected", task.ErrorCode)
	require.True(t, refundedWith(w, "platform_failure"))
}

func TestNativeTransientPollErrorsKeepTheTask(t *testing.T) {
	e, plan, w, _ := setup(t)
	ctx := context.Background()
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	poll := &pollStub{err: errors.New("connection reset")}
	resolve := func(context.Context, *model.NativeTask) (Poller, error) { return poll, nil }
	_, err = e.Poll(ctx, "req", "g", 1, resolve, nil)
	require.Error(t, err)
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "queued", task.Status)
	require.False(t, refundedWith(w, "platform_failure"))
}
