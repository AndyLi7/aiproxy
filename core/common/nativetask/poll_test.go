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
	settled := false
	for _, c := range w.commands {
		if c.Action == "settle" && c.Outcome.Kind == "failed" {
			settled = true
		}
	}
	require.False(t, settled, "an accepted provider failure must reconcile its actual bill before any refund")
}
