package nativetask

import (
	"context"
	"time"

	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
)

type dispatchObservation struct{ accepted, returned, failed bool }

// Reporting happens after work returns, including its owned defers/cleanup.
// It does not recover a panic: production panic behavior is unchanged, and
// an abnormal worker never reports normal exit. The supervisor owns no paid
// work; WaitGroup completion includes its bounded persistence attempt.
func observeDispatchWorker(work func() dispatchObservation, done chan<- dispatchObservation) {
	result := dispatchObservation{}

	normal := false
	defer func() {
		if !normal {
			result.returned = false
		}

		done <- result
	}()

	result = work()
	result.returned = true
	normal = true
}

func (e *Engine) dispatchHandoffWorker(
	ctx context.Context,
	task *model.NativeTask,
	handoff *model.NativeDispatchHandoffHandle,
) dispatchObservation {
	dispatchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), submitTimeout)
	defer cancel()

	accepted, err := handoff.Accept(dispatchCtx)
	if err != nil || !accepted {
		log.WithFields(log.Fields{"lane": "native", "task_id": task.ID}).
			Warn("native dispatch child not accepted; handoff retained")
		return dispatchObservation{}
	}

	_, err = e.dispatchInvocation(dispatchCtx, task, nil)

	return dispatchObservation{accepted: true, failed: err != nil}
}

func (e *Engine) superviseDispatchHandoff(
	ctx context.Context,
	task *model.NativeTask,
	handoff *model.NativeDispatchHandoffHandle,
) {
	done := make(chan dispatchObservation, 1)
	go observeDispatchWorker(
		func() dispatchObservation { return e.dispatchHandoffWorker(ctx, task, handoff) },
		done,
	)

	result := <-done
	recordDispatchObservation(task, handoff, result)
}

func recordDispatchObservation(
	task *model.NativeTask,
	handoff *model.NativeDispatchHandoffHandle,
	result dispatchObservation,
) {
	if !result.returned || !result.accepted {
		return
	}

	recordCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := handoff.RecordExit(recordCtx, result.failed); err != nil {
		log.WithFields(log.Fields{"lane": "native", "task_id": task.ID}).
			Warn("native dispatch exit not persisted; execution retained")
	}
}
