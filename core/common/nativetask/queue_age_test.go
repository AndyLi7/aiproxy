//nolint:testpackage // White-box coverage needs the package-private newQueueEnv fixture or implementation.
package nativetask

import (
	"context"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestExpiredWaitingNeverDispatchesBeforeRecoveryRefunds(t *testing.T) {
	env := newQueueEnv(t)
	_, err := env.submit("x1", "gx")
	require.NoError(t, err)
	_, err = env.submit("a1", "ga")
	require.NoError(t, err)

	original := time.Now().Add(-6 * time.Minute).UTC()
	require.NoError(
		t,
		env.e.DB.Model(&model.NativeTask{}).
			Where("id = ?", "a1").
			Update("enqueued_at", original).
			Error,
	)
	env.finish("x1")
	env.round()
	require.Equal(
		t,
		1,
		env.p.count(),
		"no provider submission before the overdue-wait recovery runs",
	)
	require.Equal(
		t,
		[]string{"admit:queue"},
		env.w.actionsFor("a1"),
		"no wallet attempt for expired waiting work",
	)

	for range 2 {
		_, err = env.e.RecoverOnce(
			context.Background(),
			"worker",
			time.Now().Add(time.Second),
			nil,
			nil,
		)
		require.NoError(t, err)
	}

	task := env.task("a1", "ga")
	require.Equal(t, model.QueueTimeoutCode, task.ErrorCode)
	require.True(t, task.BillingSettled)
	require.WithinDuration(t, original, *task.EnqueuedAt, time.Microsecond)
	require.Equal(
		t,
		[]string{"admit:queue", "get", "settle:failed/not_dispatched", "get"},
		env.w.actionsFor("a1"),
	)
}

func TestLateReservedRecoveryRetainsAgeAndNeverStartsProvider(t *testing.T) {
	env := newQueueEnv(t)
	_, err := env.submit("x1", "gx")
	require.NoError(t, err)

	env.w.lose["admit"] = true
	_, err = env.submit("a1", "ga")
	require.Error(t, err)
	require.Equal(t, "reserved", env.task("a1", "ga").Status)

	admitted := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Millisecond)

	env.w.mu.Lock()
	env.w.holds["native:a1"].createdAt = admitted
	env.w.mu.Unlock()
	_, err = env.e.RecoverOnce(context.Background(), "worker", time.Now(), nil, nil)
	require.NoError(t, err)

	queued := env.task("a1", "ga")
	require.Equal(t, model.NativeStatusWaiting, queued.Status)
	require.WithinDuration(t, admitted, *queued.EnqueuedAt, time.Millisecond)
	env.finish("x1")
	env.round()
	require.Equal(t, 1, env.p.count())

	for _, action := range env.w.actionsFor("a1") {
		require.NotEqual(t, "begin_attempt", action)
	}

	for range 2 {
		_, err = env.e.RecoverOnce(
			context.Background(),
			"worker",
			time.Now().Add(time.Second),
			nil,
			nil,
		)
		require.NoError(t, err)
	}

	task := env.task("a1", "ga")
	require.Equal(t, model.QueueTimeoutCode, task.ErrorCode)
	require.True(t, task.BillingSettled)
	require.WithinDuration(t, admitted, *task.EnqueuedAt, time.Millisecond)

	actions := env.w.actionsFor("a1")

	refunds := 0
	for _, action := range actions {
		if action == "settle:failed/not_dispatched" {
			refunds++
		}
	}

	require.Equal(t, 1, refunds)
}
