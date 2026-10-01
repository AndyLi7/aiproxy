package nativetask

import (
	"context"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
)

type Poller interface {
	PollNative(context.Context, string, string, *nativeresult.CompiledTask) (nativeresult.PollResult, error)
}

// ResolvePoller must bind the recorded channel and credential fingerprint; it
// must reject a rotated credential rather than polling another provider account.
type ResolvePoller func(context.Context, *model.NativeTask) (Poller, error)

func (e *Engine) Poll(ctx context.Context, id, group string, token int, resolve ResolvePoller, archive ArchiveFunc) (*model.NativeTask, error) {
	if e == nil || e.DB == nil || e.Wallet == nil {
		return nil, ErrUnavailable
	}
	task, err := model.GetNativeTask(e.DB, id, group, token)
	if err != nil {
		return nil, err
	}
	switch task.Status {
	case "result_received", "delivery_ready", "completed":
		return e.Deliver(ctx, id, group, token, archive)
	case "queued", "running":
	default:
		return task, e.SyncBilling(ctx, task)
	}
	if resolve == nil || task.UpstreamID == "" {
		return task, ErrUnavailable
	}
	if err = e.SyncBilling(ctx, task); err != nil {
		return task, err
	}
	contract, err := nativeresult.CompileTaskContract([]byte(task.FrozenContract))
	if err != nil {
		return task, err
	}
	provider, err := resolve(ctx, task)
	if err != nil {
		return task, err
	}
	if provider == nil {
		return task, ErrUnavailable
	}
	result, err := provider.PollNative(ctx, task.Endpoint, task.UpstreamID, contract)
	if err != nil {
		return task, err
	}
	if result.Status == "result_received" {
		if err = model.SaveNativeTaskResult(e.DB, id, group, token, result.Output); err != nil {
			return task, err
		}
		return e.Deliver(ctx, id, group, token, archive)
	}
	if err = model.UpdateNativePoll(e.DB, id, group, token, result.Status, result.ErrorCode); err != nil {
		return task, err
	}
	task, err = model.GetNativeTask(e.DB, id, group, token)
	if err != nil {
		return nil, err
	}
	return task, e.SyncBilling(ctx, task)
}
