package nativetask

import (
	"context"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
)

type Poller interface {
	PollNative(context.Context, string, string, *nativeresult.CompiledTask) (nativeresult.PollResult, error)
}

// Canceller is optional on a Poller (fal's client implements it). Cancellation
// is best effort: it never changes the task or its billing, its outcome is only
// logged, and a provider may still finish and bill. It is only reached through
// ResolvePoller, so it always uses the task's own bound credential.
type Canceller interface {
	CancelNative(ctx context.Context, endpoint, upstreamID string) (string, error)
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
	if permanent(err) {
		return e.rejectResult(ctx, id, group, token)
	}
	if err != nil {
		return task, err
	}
	if result.Status == "result_received" {
		if err = model.SaveNativeTaskResult(e.DB, id, group, token, result.Output); permanent(err) {
			return e.rejectResult(ctx, id, group, token)
		} else if err != nil {
			return task, err
		}
		return e.Deliver(ctx, id, group, token, archive)
	}
	if result.Status == "failed" {
		result = customerRejection(contract, result)
		logProviderFailure(task, result)
	}
	if err = model.UpdateNativePoll(e.DB, id, group, token, result.Status, result.ErrorCode, result.Issues...); err != nil {
		return task, err
	}
	task, err = model.GetNativeTask(e.DB, id, group, token)
	if err != nil {
		return nil, err
	}
	return task, e.SyncBilling(ctx, task)
}

// customerRejection keeps only the issues the customer can fix. When fal named
// registry-frozen platform controls only, the task fails as
// upstream_result_rejected instead; the log reason still names them.
func customerRejection(contract *nativeresult.CompiledTask, result nativeresult.PollResult) nativeresult.PollResult {
	if result.ErrorCode != model.InvalidParametersCode || len(result.Issues) == 0 {
		return result
	}
	result.Issues = contract.CustomerIssues(result.Issues)
	if len(result.Issues) == 0 {
		result.ErrorCode = "upstream_result_rejected"
	}
	return result
}

// logProviderFailure records an accepted task the provider failed or whose
// input it rejected, for operators. It logs identities, the provider status,
// the adaptor's sanitized type@field reason and the number of issues only:
// never credentials, provider messages or the customer's input.
func logProviderFailure(task *model.NativeTask, result nativeresult.PollResult) {
	fields := log.Fields{
		"lane":            "native",
		"task_id":         task.ID,
		"group":           task.GroupID,
		"model":           task.Model,
		"endpoint":        task.Endpoint,
		"channel_id":      task.ChannelID,
		"provider_status": result.ProviderStatus,
		"provider_reason": result.ProviderReason,
		"error_code":      result.ErrorCode,
	}
	if len(result.Issues) > 0 {
		fields["issues"] = len(result.Issues)
	}
	log.WithFields(fields).Warn("native task failed at the provider")
}
