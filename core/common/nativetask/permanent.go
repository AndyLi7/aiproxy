package nativetask

import (
	"context"
	"errors"

	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
)

// permanent reports errors that retrying the same task can never fix: the
// provider's result is invalid or too large, or an artifact can never be
// archived. Such tasks end as failed and are refunded instead of retried.
func permanent(err error) bool {
	return errors.Is(err, nativeresult.ErrInvalid) ||
		errors.Is(err, nativeresult.ErrTooLarge) ||
		errors.Is(err, ownedartifact.ErrTooLarge) ||
		errors.Is(err, ownedartifact.ErrUnsupportedSource) ||
		errors.Is(err, ownedartifact.ErrSourceRejected)
}

// rejectResult ends an accepted task whose result can never be delivered and
// settles it so the customer's prepayment is released.
func (e *Engine) rejectResult(ctx context.Context, id, group string, token int) (*model.NativeTask, error) {
	if err := model.FailAcceptedNativeTask(e.DB, id, group, token, "upstream_result_rejected"); err != nil {
		return nil, err
	}
	task, err := model.GetNativeTask(e.DB, id, group, token)
	if err != nil {
		return nil, err
	}
	return task, e.SyncBilling(ctx, task)
}
