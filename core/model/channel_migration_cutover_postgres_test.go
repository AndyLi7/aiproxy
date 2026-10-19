package model_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestPostgresAtomicCutoverFinalPinLock(t *testing.T) {
	for _, outcome := range []string{"live", "deployment-expired", "probe-expired", "pins-changed", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			db := queueLockPostgres(t)
			f := admissionFixtureOn(t, db, "")
			req, probe := migrationFixture(t, f, 3)

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			deadline := time.Now().Add(1200 * time.Millisecond)
			if outcome == "deployment-expired" {
				f.proof.ValidUntilMs = deadline.UnixMilli()
				f.refreshSyntheticProof(t)
			}

			if outcome == "probe-expired" {
				probe.ValidUntilMs = deadline.UnixMilli()
				hash, e := probe.Fingerprint()
				require.NoError(t, e)
				require.NoError(
					t,
					db.Model(&model.ChannelMigrationProbeRecord{}).
						Where("evidence_hash=?", probe.EvidenceHash).
						Update("fact_hash", hash).
						Error,
				)
			}

			_, e := f.store.PrepareMigration(ctx, req)
			require.NoError(t, e)
			before := migrationConfig(t, db)
			pool, e := db.DB()
			require.NoError(t, e)
			holder, e := pool.BeginTx(t.Context(), nil)
			require.NoError(t, e)

			defer func() {
				if err := holder.
					Rollback(); err !=
					nil &&
					!errors.
						Is(err, sql.ErrTxDone) {
					t.Error(err)
				}
			}()

			var name string
			require.NoError(
				t,
				holder.QueryRowContext(ctx, "SELECT model FROM model_configs WHERE model='example' FOR UPDATE").
					Scan(&name),
			)

			done := make(chan error, 1)
			go func() { _, err := f.store.CutoverPrepared(ctx, req); done <- err }()

			var app string
			require.NoError(
				t,
				db.Raw("SELECT current_setting('application_name')").Scan(&app).Error,
			)
			require.Eventually(t, func() bool {
				var n int64

				err := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock' AND query ILIKE '%UPDATE%model_configs%'", app).
					Scan(&n).
					Error

				return err == nil && n > 0
			}, 2*time.Second, 5*time.Millisecond)
			// The UPDATE wait is after sealTx. A second connection still observes the
			// original complete state, never the private intermediate sealed state.
			cutoverUnchanged(t, f, req, before, 0)

			if outcome == "deployment-expired" || outcome == "probe-expired" {
				if d := time.Until(deadline.Add(30 * time.Millisecond)); d > 0 {
					time.Sleep(d)
				}
			}

			if outcome == "pins-changed" {
				_, e = holder.ExecContext(
					ctx,
					"UPDATE model_configs SET config=config||' ' WHERE model='example'",
				)
				require.NoError(t, e)
			}

			if outcome == "cancelled" {
				cancel()

				select {
				case e = <-done:
					require.Error(t, e)
				case <-time.After(time.Second):
					t.Fatal("cancel did not stop commit lock wait")
				}

				require.NoError(t, holder.Rollback())
			} else {
				require.NoError(t, holder.Commit())

				e = <-done
			}

			if outcome == "live" {
				require.NoError(t, e)
				read, e := f.store.ReadPreparedMigration(t.Context(), req.Seal.OperationID)
				require.NoError(t, e)
				require.EqualValues(t, 2, read.CommitRevision)

				return
			}

			require.Error(t, e)

			if outcome == "pins-changed" {
				before += " "
			}

			cutoverUnchanged(t, f, req, before, 0)
		})
	}
}
