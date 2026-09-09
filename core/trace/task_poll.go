package trace

import (
	"context"
	"errors"
	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/labring/aiproxy/core/model"
	"sync"
	"time"
)

// BeginTaskPoll observes only the fetcher call. It never schedules, retries or settles.
func (r *Runtime) BeginTaskPoll(ctx context.Context, info *model.AsyncUsageInfo) func(bool, error) {
	noop := func(bool, error) {}
	if r == nil || !r.ready || info == nil || ctx == nil {
		return noop
	}
	lookup, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	link, err := r.store.ResolveTaskTrace(lookup, info.GroupID, info.TokenID, info.ChannelID, info.UpstreamID, time.Now())
	cancel()
	if err != nil || link.AsyncUsageID != info.ID {
		r.correlationFailures.Add(1)
		return noop
	}
	s := requesttrace.NewTaskPollSession(info.RequestID, r.writer.Submit)
	if !s.BindStoredTask(info.GroupID, link.TraceID, link.ParentSpanID) {
		r.correlationFailures.Add(1)
		return noop
	}
	var once sync.Once
	return func(completed bool, fetchErr error) {
		once.Do(func() {
			summaryCtx, cancelSummary := context.WithTimeout(context.WithoutCancel(ctx), 100*time.Millisecond)
			if r.store.RecordTaskPoll(summaryCtx, info.ID, info.GroupID, time.Now()) != nil {
				r.correlationFailures.Add(1)
			}
			cancelSummary()
			status := requesttrace.StatusSuccess
			if fetchErr != nil {
				status = requesttrace.StatusError
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				status = requesttrace.StatusTimeout
			} else if ctx.Err() != nil {
				status = requesttrace.StatusCancelled
			}
			if completed && ctx.Err() == nil {
				s.Begin(requesttrace.StageAsyncObservedResult, requesttrace.Attributes{}).Finish(status)
			}
			s.Finish(status)
		})
	}
}
