package nativetask

import (
	"context"
	"time"

	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
)

// The callback runs outside all database transactions. Existing task/queue
// transitions retain their own CAS and business leases. A panic leaves active
// evidence; a normal return is never evidence that all downstream work ended.
func (e *Engine) runTaskExecution(
	ctx context.Context,
	task *model.NativeTask,
	kind model.NativeExecutionKind,
	work func() (*model.NativeTask, error),
) (*model.NativeTask, error) {
	owner, _ := ctx.Value(nativeRecoveryOwnerKey{}).(string)
	until, _ := ctx.Value(nativeRecoveryExecutionUntilKey{}).(int64)

	handle, err := model.BeginNativeTaskExecution(ctx, e.DB, task, kind, owner, until)
	if err != nil {
		return task, err
	}

	result, workErr := work()
	if handle != nil {
		recordCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = handle.RecordReturn(recordCtx, workErr != nil)

		cancel()

		if err != nil {
			log.WithFields(log.Fields{"lane": "native", "task_id": task.ID, "scope": kind}).
				Warn("native invocation return not persisted; execution retained")
		}
	}

	return result, workErr
}
