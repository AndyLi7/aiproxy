package fal

import (
	"context"
	"errors"
	"net/http"

	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/model"
)

// Outcomes of a fal queue cancel request, shared by the native and image
// lanes. They are only logged: by the time a cancel is sent the task has
// already failed with generation_timeout and its hold is refunded.
const (
	NativeCancelRequested        = "cancellation_requested"
	NativeCancelAlreadyCompleted = "already_completed"
	NativeCancelNotFound         = "not_found"
)

// Cancel asks fal once to stop an accepted request with
// PUT {queue path}/requests/{id}/cancel (empty body, redirects never followed).
// The path comes from the same validated sources as Poll: the frozen task
// contract when there is one, else the owner/app root; never a provider-supplied
// cancel_url. Like Poll, an explicit 405 on a contract's full model path is
// retried once at the owner/app root, which some fal apps require.
//
// fal answers 202 CANCELLATION_REQUESTED, 400 ALREADY_COMPLETED or
// 404 NOT_FOUND; those are outcomes, and only transport errors and other
// statuses are errors. It is best effort: a running request may still finish
// and be billed, and callers never retry it.
func (c *Client) Cancel(ctx context.Context, modelName, id string, frozenContracts ...[]byte) (string, error) {
	root, err := endpoint(modelName)
	if err != nil {
		return "", err
	}
	if !segment.MatchString(id) {
		return "", errors.New("invalid fal request id")
	}
	if len(frozenContracts) > 1 {
		return "", errors.New("multiple task contracts")
	}

	legacy := root + "/requests/" + id
	path := legacy
	if len(frozenContracts) == 1 && registryvalidation.HasProviderContracts(frozenContracts[0]) {
		if _, path, err = registryvalidation.FrozenFalQueuePaths(frozenContracts[0], modelName, id); err != nil {
			return "", err
		}
	}

	_, code, err := c.nativeRequest(ctx, http.MethodPut, path+"/cancel", nil)
	if code == http.StatusMethodNotAllowed && path != legacy {
		_, code, err = c.nativeRequest(ctx, http.MethodPut, legacy+"/cancel", nil)
	}
	switch {
	case err == nil:
		return NativeCancelRequested, nil
	case code == http.StatusBadRequest:
		return NativeCancelAlreadyCompleted, nil
	case code == http.StatusNotFound:
		return NativeCancelNotFound, nil
	}
	return "", err
}

// CancelNative cancels a native model task at the owner/app queue root that
// PollNative reads.
func (c *Client) CancelNative(ctx context.Context, modelName, id string) (string, error) {
	return c.Cancel(ctx, modelName, id)
}

// CancelImage implements adaptor.ImageTaskCanceller with the account and queue
// base that accepted the task, the same pair PollImage uses. The worker only
// offers it after the task's key fingerprint and channel type checks passed.
func (a *Adaptor) CancelImage(
	ctx context.Context,
	ch *model.Channel,
	info *model.AsyncUsageInfo,
	task *model.ImageTask,
) (string, error) {
	return (&Client{BaseURL: info.BaseURL, Key: ch.Key}).Cancel(
		ctx,
		task.UpstreamModel,
		task.UpstreamID,
		[]byte(task.ValidationContract),
	)
}
