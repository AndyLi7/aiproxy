package model_test

import (
	"context"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestPostgresAdmissionExecutionReturnRechecksAfterLocks(t *testing.T) {
	for _, scenario := range []string{"live", "executor-replaced", "use-replaced", "storage-restored"} {
		t.Run(scenario, func(t *testing.T) {
			db := queueLockPostgres(t)
			f := runtimeAdmissionOnFixture(t, admissionFixtureOn(t, db, ""))
			installExecutionJournal(t, db)
			use, handle, err := relayExecution(t, f)
			require.NoError(t, err)
			pool, err := db.DB()
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			holder, err := pool.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer holder.Rollback()
			if scenario == "storage-restored" {
				var id int
				require.NoError(t, holder.QueryRowContext(ctx, "SELECT id FROM channel_admission_controls WHERE id=1 FOR UPDATE").Scan(&id))
			} else {
				var id string
				require.NoError(t, holder.QueryRowContext(ctx, "SELECT id FROM channel_admission_executions WHERE use_id=$1 FOR UPDATE", use.ID).Scan(&id))
			}
			done := make(chan error, 1)
			go func() { done <- handle.RecordReturn(ctx, model.RelayExecutionReturned) }()
			var application string
			require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&application).Error)
			require.Eventually(t, func() bool {
				var count int64
				e := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", application).Scan(&count).Error
				return e == nil && count > 0
			}, 2*time.Second, 5*time.Millisecond)
			switch scenario {
			case "executor-replaced":
				_, err = holder.ExecContext(ctx, "UPDATE channel_admission_executions SET instance_id=$1 WHERE use_id=$2", "new-executor", use.ID)
			case "use-replaced":
				_, err = holder.ExecContext(ctx, "UPDATE channel_admission_uses SET request_hash=$1 WHERE id=$2", use.ID, use.ID)
			case "storage-restored":
				_, err = holder.ExecContext(ctx, "UPDATE channel_admission_controls SET storage_generation=$1 WHERE id=1", "restored-storage")
			}
			require.NoError(t, err)
			require.NoError(t, holder.Commit())
			err = <-done
			row := executionRows(t, db)[0]
			if scenario == "live" {
				require.NoError(t, err)
				require.Equal(t, "returned", row.State)
			} else {
				require.Error(t, err)
				require.Equal(t, "active", row.State)
				require.Zero(t, row.ReturnedAtMs)
			}
			require.Equal(t, "unproven", row.ContinuationState)
			require.Equal(t, "open", runtimeUses(t, db)[0].State)
		})
	}
}
