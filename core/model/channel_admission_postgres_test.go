package model_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestPostgresAdmissionProofRecheckedAfterChannelLock(t *testing.T) {
	for _, outcome := range []string{"expired", "revoked", "live"} {
		t.Run(outcome, func(t *testing.T) {
			db := queueLockPostgres(t)
			f := admissionFixtureOn(t, db, "")
			deadline := time.Now().Add(time.Second)
			if outcome == "expired" {
				f.proof.ValidUntilMs = deadline.UnixMilli()
				f.refreshSyntheticProof(t)
			}
			pool, err := db.DB()
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			holder, err := pool.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer holder.Rollback()
			var id int
			require.NoError(t, holder.QueryRowContext(ctx, "SELECT id FROM channels WHERE id=$1 FOR UPDATE", 1).Scan(&id))
			done := make(chan error, 1)
			go func() { _, err := f.store.Seal(ctx, f.seal("seal", 0, 1, 2, "")); done <- err }()
			var application string
			require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&application).Error)
			require.Eventually(t, func() bool {
				var count int64
				err := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", application).Scan(&count).Error
				return err == nil && count > 0
			}, 2*time.Second, 5*time.Millisecond)
			if outcome == "expired" {
				if delay := time.Until(deadline.Add(100 * time.Millisecond)); delay > 0 {
					time.Sleep(delay)
				}
			}
			if outcome == "revoked" {
				require.NoError(t, db.Model(&model.ChannelAdmissionProof{}).Where("id = ?", f.proof.ProofID).Update("revoked", true).Error)
			}
			released := time.Now()
			require.NoError(t, holder.Commit())
			err = <-done
			var count int64
			require.NoError(t, db.Model(&model.ChannelAdmissionBarrier{}).Count(&count).Error)
			if outcome == "live" {
				require.NoError(t, err)
				require.Equal(t, int64(1), count)
				var row model.ChannelAdmissionBarrier
				require.NoError(t, db.First(&row).Error)
				require.GreaterOrEqual(t, row.RecordedAtMs, released.Add(-20*time.Millisecond).UnixMilli(), "journal timestamp is sampled after the lock, never a drain start")
			} else {
				require.ErrorIs(t, err, model.ErrChannelAdmissionUnavailable)
				require.Zero(t, count)
				require.NoError(t, db.Model(&model.ChannelAdmissionFamily{}).Count(&count).Error)
				require.Zero(t, count)
			}
		})
	}
}

func TestPostgresAdmissionMigrationDraftRemainsInactive(t *testing.T) {
	db := queueLockPostgres(t)
	script, err := os.ReadFile("../migrations/channel-admission-v1/postgres.sql")
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	conn, err := pool.Conn(context.Background())
	require.NoError(t, err)
	defer conn.Close()
	for range 2 {
		require.NoError(t, conn.Raw(func(driverConn any) error {
			_, execErr := driverConn.(*stdlib.Conn).Conn().PgConn().Exec(context.Background(), string(script)).ReadAll()
			return execErr
		}))
	}
	require.True(t, model.ChannelAdmissionStorageReady(db))
	var control model.ChannelAdmissionControl
	require.NoError(t, db.First(&control, 1).Error)
	require.Empty(t, control.Epoch)
	require.Empty(t, control.ActiveProofID)
	require.Zero(t, control.EpochSequence)
}
