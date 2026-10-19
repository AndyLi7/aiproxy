package model_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestPostgresMigrationReadFactsNoDML(t *testing.T) {
	db := queueLockPostgres(t)
	f := admissionFixtureOn(t, db, "read-facts")
	req, _ := migrationFixture(t, f, 3)
	ctx := context.Background()
	_, err := f.store.PrepareMigration(ctx, req)
	require.NoError(t, err)
	seal, err := f.store.SealPrepared(ctx, req)
	require.NoError(t, err)
	_, err = f.store.CommitPreparedPins(ctx, req, seal.Revision)
	require.NoError(t, err)
	require.NoError(
		t,
		db.Exec(
			`CREATE FUNCTION read_facts_no_writes() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'read facts is read only'; END $$`,
		).Error,
	)

	for _, table := range []string{"channels", "model_configs", "channel_admission_controls", "channel_admission_proofs", "channel_admission_barriers", "channel_admission_families", "channel_admission_uses", "channel_migration_protocols", "channel_migration_prepareds", "channel_migration_probe_records"} {
		require.NoError(
			t,
			db.Exec(
				fmt.Sprintf(
					"CREATE TRIGGER read_facts_no_writes BEFORE INSERT OR UPDATE OR DELETE ON %s FOR EACH STATEMENT EXECUTE FUNCTION read_facts_no_writes()",
					table,
				),
			).Error,
		)
	}

	var exclusive atomic.Bool
	require.NoError(
		t,
		db.Callback().Query().After("gorm:query").Register("read-no-exclusive", func(tx *gorm.DB) {
			if strings.Contains(tx.Statement.SQL.String(), "FOR UPDATE") {
				exclusive.Store(true)
			}
		}),
	)
	require.Equal(t, "ready", f.store.ReadMigrationCommandProtocol(ctx).State)
	require.Equal(
		t,
		"ready",
		f.store.ReadMigrationSnapshotFacts(ctx, req.Seal.From, req.Seal.To).State,
	)
	got := f.store.ReadPreparedMigrationFacts(ctx, req.Seal.OperationID)
	require.Equal(t, "ready", got.State)
	require.Equal(t, "committed", got.Readback.Receipt.State)
	require.Equal(t, "absent", f.store.ReadPreparedMigrationFacts(ctx, "absent-op").State)
	require.False(
		t,
		exclusive.Load(),
		"snapshot validators must not retain command UPDATE row locks",
	)
}

func TestPostgresMigrationReadFactsLockWaitSnapshot(t *testing.T) {
	for _, change := range []string{"rollback", "head", "receipt", "probe-revoked", "member-added", "member-deleted", "epoch", "cancel"} {
		t.Run(change, func(t *testing.T) {
			db := queueLockPostgres(t)
			f := admissionFixtureOn(t, db, "read-lock")
			req, _ := migrationFixture(t, f, 3)
			ctx := context.Background()
			_, err := f.store.PrepareMigration(ctx, req)
			require.NoError(t, err)
			seal, err := f.store.SealPrepared(ctx, req)
			require.NoError(t, err)
			_, err = f.store.CommitPreparedPins(ctx, req, seal.Revision)
			require.NoError(t, err)
			pool, err := db.DB()
			require.NoError(t, err)
			holder, err := pool.BeginTx(t.Context(), nil)
			require.NoError(t, err)

			defer func() {
				if err := holder.
					Rollback(); err !=
					nil &&
					!errors.
						Is(err, sql.ErrTxDone) {
					t.Error(
						err,
					)
				}
			}()

			_, err = holder.ExecContext(t.Context(),

				"UPDATE channel_admission_controls SET revision=revision+1 WHERE id=1",
			)
			require.NoError(t, err)

			switch change {
			case "head":
				_, err = holder.ExecContext(t.Context(),

					"UPDATE channel_admission_families SET operation_id='new-head'",
				)
			case "receipt":
				_, err = holder.ExecContext(t.Context(),

					"DELETE FROM channel_migration_prepareds")
			case "probe-revoked":
				_, err = holder.ExecContext(t.Context(),

					"UPDATE channel_migration_probe_records SET revoked=true")
			case "member-added":
				_, err = holder.ExecContext(t.Context(),

					"INSERT INTO model_configs(model,config,plugin) VALUES('new-member','{}','{}')",
				)
			case "member-deleted":
				_, err = holder.ExecContext(t.Context(),

					"DELETE FROM model_configs")
			case "epoch":
				_, err = holder.ExecContext(t.Context(),

					"UPDATE channel_admission_controls SET epoch='new-epoch'")
			}

			require.NoError(t, err)

			child, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			done := make(chan model.MigrationReadResult, 1)
			go func() { done <- f.store.ReadPreparedMigrationFacts(child, req.Seal.OperationID) }()

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
			}, 2*time.Second, 5*time.Millisecond)

			switch change {
			case "rollback":
				require.NoError(t, holder.Rollback())
			case "cancel":
				cancel()
			default:
				require.NoError(t, holder.Commit())
			}

			var got model.MigrationReadResult
			select {
			case got = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("reader did not stop waiting")
			}

			if change == "rollback" {
				require.Equal(t, "ready", got.State)
			} else {
				require.NotEqual(t, "ready", got.State)
				require.NotEqual(t, "absent", got.State)
				require.Nil(t, got.Readback)
			}

			if change != "rollback" && change != "cancel" {
				again := f.store.ReadPreparedMigrationFacts(ctx, req.Seal.OperationID)
				require.NotEqual(t, "ready", again.State)
				require.NotEqual(t, "absent", again.State)
			}
		})
	}
}
