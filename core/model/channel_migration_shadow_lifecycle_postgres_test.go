package model_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestPostgresMigrationShadowLifecycleLockWait(t *testing.T) {
	for _, mode := range []string{"prepare-source", "seal-source", "cancel-config", "cancel-target", "cancel-sealed", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			db := queueLockPostgres(t)
			f := admissionFixtureOn(t, db, "lifecycle-lock")
			source, receipt, req := lifecycleFixture(t, f)

			ctx := context.Background()
			if mode == "seal-source" || mode == "cancel-sealed" {
				_, e := f.store.PrepareMigration(ctx, req)
				require.NoError(t, e)
			}

			pool, e := db.DB()
			require.NoError(t, e)
			holder, e := pool.BeginTx(t.Context(), nil)
			require.NoError(t, e)

			defer func() {
				if err := holder.
					Rollback(); err !=
					nil &&
					!errors.
						Is(err,
							sql.ErrTxDone,
						) {
					t.Error(
						err,
					)
				}
			}()

			// Hold the exact source row for prepare/seal, or control for cancel.
			if mode == "prepare-source" || mode == "seal-source" {
				_, e = holder.ExecContext(t.Context(),

					"UPDATE model_configs SET rpm=rpm WHERE model='example'")
			} else {
				_, e = holder.ExecContext(t.Context(),

					"UPDATE channel_admission_controls SET revision=revision WHERE id=1",
				)
			}

			require.NoError(t, e)

			done := make(chan error, 1)
			go func() {
				var e error
				switch mode {
				case "prepare-source", "rollback":
					_, e = f.store.PrepareMigration(ctx, req)
				case "seal-source":
					_, e = f.store.SealPrepared(ctx, req)
				default:
					_, e = f.store.CancelMigrationShadow(
						ctx,
						source.OperationID,
						receipt.RequestHash,
						receipt.CheckpointHash,
					)
				}

				done <- e
			}()

			var app string
			require.NoError(
				t,
				db.Raw("SELECT current_setting('application_name')").Scan(&app).Error,
			)
			require.Eventually(t, func() bool {
				var n int64

				return db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).
					Scan(&n).
					Error == nil &&
					n > 0
			}, 3*time.Second, 5*time.Millisecond)

			switch mode {
			case "rollback":
				require.NoError(t, holder.Rollback())
			case "cancel-target":
				_, e = holder.ExecContext(t.Context(),

					"UPDATE channels SET status=1 WHERE id=$1",
					receipt.Shadow.ChannelID,
				)
				require.NoError(t, e)
				require.NoError(t, holder.Commit())
			case "cancel-sealed":
				_, e = holder.ExecContext(
					t.Context(),
					"UPDATE channel_migration_prepareds SET state='sealed',seal_revision=99 WHERE operation_id=$1",
					source.OperationID,
				)
				require.NoError(t, e)
				require.NoError(t, holder.Commit())
			default:
				_, e = holder.ExecContext(t.Context(),

					"UPDATE model_configs SET rpm=999")
				require.NoError(t, e)
				require.NoError(t, holder.Commit())
			}

			select {
			case e = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("command failed to join")
			}

			if mode == "rollback" || mode == "cancel-config" {
				require.NoError(t, e)
			} else {
				require.Error(t, e)
			}

			r := lifecycleRow(t, f, source.OperationID)
			switch mode {
			case "cancel-config":
				require.Equal(t, "cancelled", r.LifecycleState)
			case "rollback":
				require.Equal(t, "consumed", r.LifecycleState)
			case "prepare-source":
				require.Equal(t, "created", r.LifecycleState)
			}
		})
	}
}

func TestPostgresMigrationShadowLifecycleConcurrent(t *testing.T) {
	for _, action := range []string{"prepare", "seal", "cancel"} {
		t.Run(action, func(t *testing.T) {
			db := queueLockPostgres(t)
			f := admissionFixtureOn(t, db, "lifecycle-concurrent")
			source, receipt, req := lifecycleFixture(t, f)

			ctx := context.Background()
			if action == "seal" {
				_, e := f.store.PrepareMigration(ctx, req)
				require.NoError(t, e)
			}

			var wg sync.WaitGroup

			start := make(chan struct{})
			errs := make(chan error, 2)

			wg.Add(2)
			go func() {
				defer wg.Done()

				<-start

				_, e := f.store.CancelMigrationShadow(
					ctx,
					source.OperationID,
					receipt.RequestHash,
					receipt.CheckpointHash,
				)
				errs <- e
			}()
			go func() {
				defer wg.Done()

				<-start

				var e error
				switch action {
				case "prepare":
					_, e = f.store.PrepareMigration(ctx, req)
				case "seal":
					_, e = f.store.SealPrepared(ctx, req)
				case "cancel":
					_, e = f.store.CancelMigrationShadow(
						ctx,
						source.OperationID,
						receipt.RequestHash,
						receipt.CheckpointHash,
					)
				}

				errs <- e
			}()

			close(start)
			wg.Wait()
			close(errs)

			success := 0
			for e := range errs {
				if e == nil {
					success++
				}
			}

			require.GreaterOrEqual(t, success, 1)
			r := lifecycleRow(t, f, source.OperationID)

			var p model.ChannelMigrationPrepared

			e := db.First(&p, "operation_id=?", source.OperationID).Error
			if r.LifecycleState == "cancelled" {
				if e == nil {
					require.Equal(t, "abandoned", p.State)
				}

				_, e = f.store.SealPrepared(ctx, req)
				require.Error(t, e)
			} else {
				require.Equal(t, "consumed", r.LifecycleState)
				require.NoError(t, e)
				require.Equal(t, "sealed", p.State)
				require.Equal(t, 1, success)
			}
		})
	}
}

func TestPostgresMigrationShadowLifecycleReadNoDML(t *testing.T) {
	db := queueLockPostgres(t)
	f := admissionFixtureOn(t, db, "lifecycle-read")
	source, receipt, req := lifecycleFixture(t, f)
	_, e := f.store.PrepareMigration(context.Background(), req)
	require.NoError(t, e)
	require.NoError(
		t,
		db.Exec(
			"CREATE FUNCTION lifecycle_deny_write() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'unexpected write'; END; $$ LANGUAGE plpgsql",
		).Error,
	)

	for _, table := range []string{"channels", "model_configs", "channel_admission_controls", "channel_migration_shadow_resources", "channel_migration_prepareds", "channel_migration_probe_records"} {
		require.NoError(
			t,
			db.Exec(
				fmt.Sprintf(
					"CREATE TRIGGER lifecycle_no_write BEFORE INSERT OR UPDATE OR DELETE ON %s FOR EACH STATEMENT EXECUTE FUNCTION lifecycle_deny_write()",
					table,
				),
			).Error,
		)
	}

	r, e := f.store.ReadMigrationShadowLifecycle(
		context.Background(),
		source.OperationID,
		receipt.CheckpointHash,
	)
	require.NoError(t, e)
	require.Equal(t, "consumed", r.State)
}

func TestPostgresMigrationShadowLifecycleRollback(t *testing.T) {
	for _, action := range []string{"consume", "cancel"} {
		t.Run(action, func(t *testing.T) {
			db := queueLockPostgres(t)
			f := admissionFixtureOn(t, db, "lifecycle-rollback")
			source, receipt, req := lifecycleFixture(t, f)
			ctx := context.Background()

			expected := "created"
			if action == "cancel" {
				_, e := f.store.PrepareMigration(ctx, req)
				require.NoError(t, e)

				expected = "consumed"
			}

			var before model.ChannelAdmissionControl
			require.NoError(t, db.First(&before, 1).Error)
			require.NoError(
				t,
				db.Exec(
					"CREATE FUNCTION lifecycle_rollback() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'fixture rollback'; END; $$ LANGUAGE plpgsql",
				).Error,
			)
			require.NoError(
				t,
				db.Exec(
					"CREATE TRIGGER lifecycle_fail BEFORE UPDATE ON channel_migration_shadow_resources FOR EACH ROW EXECUTE FUNCTION lifecycle_rollback()",
				).Error,
			)

			var e error
			if action == "consume" {
				_, e = f.store.PrepareMigration(ctx, req)
			} else {
				_, e = f.store.CancelMigrationShadow(
					ctx,
					source.OperationID,
					receipt.RequestHash,
					receipt.CheckpointHash,
				)
			}

			require.Error(t, e)
			r := lifecycleRow(t, f, source.OperationID)
			require.Equal(t, expected, r.LifecycleState)

			var after model.ChannelAdmissionControl
			require.NoError(t, db.First(&after, 1).Error)
			require.Equal(t, before.Revision, after.Revision)

			var rows []model.ChannelMigrationPrepared
			require.NoError(t, db.Find(&rows).Error)

			if action == "consume" {
				require.Empty(t, rows)
			} else {
				require.Len(t, rows, 1)
				require.Equal(t, "prepared", rows[0].State)
			}
		})
	}
}

func TestPostgresMigrationShadowLifecycleUpgrade(t *testing.T) {
	db := queueLockPostgres(t)
	f := admissionFixtureOn(t, db, "lifecycle-upgrade")
	source, receipt, req := lifecycleFixture(t, f)

	before := lifecycleRow(t, f, source.OperationID)
	for _, column := range []string{"lifecycle_state", "lifecycle_revision", "prepared_request_hash", "prepared_probe_hash", "prepared_probe_fact_hash", "prepared_at_ms", "cancelled_at_ms"} {
		require.NoError(
			t,
			db.Exec("ALTER TABLE channel_migration_shadow_resources DROP COLUMN "+column).Error,
		)
	}

	require.NoError(
		t,
		db.Exec("ALTER TABLE channel_migration_prepareds DROP COLUMN shadow_checkpoint_hash").Error,
	)
	require.NoError(t, db.Exec("UPDATE channel_migration_shadow_protocols SET version=1").Error)

	raw, e := os.ReadFile("../../docs/sql/b2-shadow-lifecycle-v2-upgrade-postgres-draft.sql")
	require.NoError(t, e)
	pool, e := db.DB()
	require.NoError(t, e)
	conn, e := pool.Conn(context.Background())
	require.NoError(t, e)

	defer conn.Close()
	defer func() {
		if _, err := conn.ExecContext(context.
			Background(), "ROLLBACK",
		); err != nil {
			t.Error(err)
		}
	}()

	for sql := range strings.SplitSeq(string(raw), ";") {
		if strings.TrimSpace(sql) != "" {
			_, e = conn.ExecContext(context.Background(), sql)
			require.NoError(t, e)
		}
	}

	after := lifecycleRow(t, f, source.OperationID)
	require.Equal(t, before.RequestJSON, after.RequestJSON)
	require.Equal(t, before.CheckpointHash, after.CheckpointHash)
	require.Equal(t, "unknown", after.LifecycleState)

	_, e = f.store.PrepareMigration(context.Background(), req)
	require.Error(t, e)
	_, e = f.store.CancelMigrationShadow(
		context.Background(),
		source.OperationID,
		receipt.RequestHash,
		receipt.CheckpointHash,
	)
	require.Error(t, e)
}

func TestPostgresMigrationShadowLifecycleLegacyPrepared(t *testing.T) {
	db := queueLockPostgres(t)
	f := admissionFixtureOn(t, db, "lifecycle-legacy")
	req, _ := migrationFixture(t, f, 3)
	ctx := context.Background()

	require.NoError(
		t,
		db.Exec("ALTER TABLE channel_migration_prepareds DROP COLUMN shadow_checkpoint_hash").Error,
	)

	p, e := f.store.PrepareMigration(ctx, req)
	require.NoError(t, e)
	require.Empty(t, p.ShadowCheckpointHash)

	seal, e := f.store.SealPrepared(ctx, req)
	require.NoError(t, e)
	_, e = f.store.CommitPreparedPins(ctx, req, seal.Revision)
	require.NoError(t, e)
	_, e = f.store.ReadPreparedMigration(ctx, req.Seal.OperationID)
	require.NoError(t, e)
}
