package nativetask

import (
	"context"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"testing"
	"time"
)

func TestNativeRecoveryCancelledBeforeAndDuringClaim(t *testing.T) {
	for _, during := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "during-db-query"}[during], func(t *testing.T) {
			e, plan, _, provider := setup(t)
			_, err := e.Submit(t.Context(), "cancel-recovery", "g", 1, body, plan)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if during {
				require.NoError(t, e.DB.Callback().Query().Before("gorm:query").Register("cancel-recovery", func(tx *gorm.DB) {
					if tx.Statement.Table == "native_tasks" {
						cancel()
					}
				}))
			} else {
				cancel()
			}
			report, err := e.RecoverOnce(ctx, "worker", time.Now(), nil, nil)
			require.ErrorIs(t, err, context.Canceled)
			require.Zero(t, report.Claimed)
			if during {
				require.NoError(t, e.DB.Callback().Query().Remove("cancel-recovery"))
			}
			task, err := model.GetNativeTask(e.DB, "cancel-recovery", "g", 1)
			require.NoError(t, err)
			require.Empty(t, task.RecoveryOwner)
			require.Zero(t, task.RecoveryUntil)
			require.Equal(t, 1, provider.calls)
		})
	}
}

func TestNativeRecoveryCancellationDoesNotClaimNextItem(t *testing.T) {
	e, plan, wallet, provider := setup(t)
	for _, id := range []string{"cancel-first", "leave-second"} {
		wallet.claimed = false
		_, err := e.Submit(t.Context(), id, "g", 1, body, plan)
		require.NoError(t, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	report, err := e.RecoverOnce(ctx, "worker", time.Now(), func(context.Context, *model.NativeTask) (Poller, error) {
		calls++
		cancel()
		return nil, context.Canceled
	}, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls)
	require.Equal(t, 1, report.Claimed)
	var rows []model.NativeTask
	require.NoError(t, e.DB.Find(&rows).Error)
	require.Len(t, rows, 2)
	untouched := 0
	for _, row := range rows {
		if row.RecoveryUntil == 0 {
			untouched++
		}
	}
	require.GreaterOrEqual(t, untouched, 1)
	require.Equal(t, 2, provider.calls)
}
