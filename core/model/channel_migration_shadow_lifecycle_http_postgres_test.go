package model_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestPostgresMigrationShadowLifecycleHTTPReadNoDML(t *testing.T) {
	db := queueLockPostgres(t)
	f := admissionFixtureOn(t, db, "lifecycle-http-read")
	source, _, req := lifecycleFixture(t, f)
	_, e := f.store.PrepareMigration(context.Background(), req)
	require.NoError(t, e)
	require.NoError(
		t,
		db.Exec(
			"CREATE FUNCTION http_lifecycle_deny_write() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'unexpected write'; END; $$ LANGUAGE plpgsql",
		).Error,
	)

	for _, table := range []string{"channels", "model_configs", "channel_admission_controls", "channel_migration_shadow_resources", "channel_migration_prepareds", "channel_migration_probe_records"} {
		require.NoError(
			t,
			db.Exec(
				fmt.Sprintf(
					"CREATE TRIGGER http_lifecycle_no_write BEFORE INSERT OR UPDATE OR DELETE ON %s FOR EACH STATEMENT EXECUTE FUNCTION http_lifecycle_deny_write()",
					table,
				),
			).Error,
		)
	}

	r := lifecycleHTTPRouter(t, f)
	_, got, _ := lifecycleHTTPCall(
		t,
		r,
		"GET",
		"/api/channel-migrations/operations/"+source.OperationID+"/shadow/lifecycle",
		"",
		config.AdminKey,
	)
	require.Equal(t, "ready", got.State)
	require.Equal(t, "consumed", got.Observation.Lifecycle.State)
	require.Equal(t, "prepared", got.Observation.PreparedBinding.State)
	_, absent, _ := lifecycleHTTPCall(
		t,
		r,
		"GET",
		"/api/channel-migrations/operations/absent/shadow/lifecycle",
		"",
		config.AdminKey,
	)
	require.Equal(t, "absent", absent.State)
}

func TestPostgresMigrationShadowLifecycleHTTPControlSnapshot(t *testing.T) {
	for _, mode := range []string{"rollback", "cancel", "binding-corrupt"} {
		t.Run(mode, func(t *testing.T) {
			db := queueLockPostgres(t)
			f := admissionFixtureOn(t, db, "lifecycle-http-snapshot")
			source, receipt, req := lifecycleFixture(t, f)
			_, e := f.store.PrepareMigration(context.Background(), req)
			require.NoError(t, e)
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

							sql.ErrTxDone) {
					t.Error(err)
				}
			}()

			_, e = holder.ExecContext(t.Context(),

				"UPDATE channel_admission_controls SET revision=revision WHERE id=1")
			require.NoError(t, e)

			done := make(chan model.MigrationShadowLifecycleResult, 1)
			go func() { done <- f.store.ReadMigrationShadowLifecycleFacts(context.Background(), source.OperationID) }()

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

			if mode == "binding-corrupt" {
				_, e = holder.ExecContext(
					t.Context(),
					"UPDATE channel_migration_prepareds SET shadow_checkpoint_hash='' WHERE operation_id=$1",
					source.OperationID,
				)
				require.NoError(t, e)
				_, e = holder.ExecContext(t.Context(),

					"UPDATE channel_admission_controls SET revision=revision+1 WHERE id=1",
				)
				require.NoError(t, e)
				require.NoError(t, holder.Commit())
			} else {
				require.NoError(t, holder.Rollback())
			}

			var got model.MigrationShadowLifecycleResult
			select {
			case got = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("read did not join")
			}

			if mode == "binding-corrupt" {
				require.NotEqual(t, "ready", got.State)
				require.NotEqual(t, "absent", got.State)

				again := f.store.ReadMigrationShadowLifecycleFacts(
					context.Background(),
					source.OperationID,
				)
				require.Equal(t, "conflict", again.State)
				require.Nil(t, again.Observation)

				return
			}

			require.Equal(t, "ready", got.State)
			require.Equal(t, "consumed", got.Observation.Lifecycle.State)
			require.Equal(t, "prepared", got.Observation.PreparedBinding.State)

			if mode == "cancel" {
				result := f.store.CancelMigrationShadowFacts(
					context.Background(),
					source.OperationID,
					receipt.RequestHash,
					receipt.CheckpointHash,
				)
				require.Equal(t, "ready", result.State)
				require.Equal(t, "cancelled", result.Observation.Lifecycle.State)
				require.Equal(t, "abandoned", result.Observation.PreparedBinding.State)
				require.Equal(
					t,
					result,
					f.store.ReadMigrationShadowLifecycleFacts(
						context.Background(),
						source.OperationID,
					),
				)
			}
		})
	}
}

func TestPostgresMigrationShadowLifecycleHTTPCancelResponseRollback(t *testing.T) {
	db := queueLockPostgres(t)
	f := admissionFixtureOn(t, db, "lifecycle-http-rollback")
	source, receipt, req := lifecycleFixture(t, f)
	_, e := f.store.PrepareMigration(context.Background(), req)
	require.NoError(t, e)
	original := lifecycleRow(t, f, source.OperationID)
	// The cancellation write changes the row; a corrupt after-write reverse
	// binding must fail the response producer and roll back the whole operation.
	require.NoError(t, db.Exec(`CREATE FUNCTION corrupt_http_cancel() RETURNS trigger AS $$ BEGIN
 IF NEW.lifecycle_state='cancelled' THEN UPDATE channel_migration_prepareds SET shadow_checkpoint_hash='' WHERE operation_id=NEW.operation_id; END IF; RETURN NEW; END; $$ LANGUAGE plpgsql`).Error)
	require.NoError(
		t,
		db.Exec(
			`CREATE TRIGGER corrupt_http_cancel AFTER UPDATE ON channel_migration_shadow_resources FOR EACH ROW EXECUTE FUNCTION corrupt_http_cancel()`,
		).Error,
	)
	r := lifecycleHTTPRouter(t, f)
	_, got, _ := lifecycleHTTPCall(
		t,
		r,
		"POST",
		"/api/channel-migrations/operations/"+source.OperationID+"/shadow/cancel",
		lifecycleCancelJSON(t, receipt),
		config.AdminKey,
	)
	require.Equal(t, "conflict", got.State)
	require.Nil(t, got.Observation)
	require.Equal(t, original, lifecycleRow(t, f, source.OperationID))

	var p model.ChannelMigrationPrepared
	require.NoError(t, db.First(&p, "operation_id=?", source.OperationID).Error)
	require.Equal(t, "prepared", p.State)
	require.Equal(t, receipt.CheckpointHash, p.ShadowCheckpointHash)
}

func TestPostgresMigrationShadowLifecycleHTTPReadWaitsAtomicCancel(t *testing.T) {
	db := queueLockPostgres(t)
	f := admissionFixtureOn(t, db, "lifecycle-http-cancel-race")
	source, receipt, req := lifecycleFixture(t, f)
	_, e := f.store.PrepareMigration(context.Background(), req)
	require.NoError(t, e)

	entered, release := make(chan struct{}), make(chan struct{})

	require.NoError(
		t,
		db.Callback().
			Update().
			After("gorm:update").
			Register("pause_http_cancel", func(tx *gorm.DB) {
				if tx.Statement.Table == "channel_migration_shadow_resources" {
					close(entered)
					<-release
				}
			}),
	)
	defer func() {
		if err := db.
			Callback().Update().
			Remove("pause_http_cancel"); err != nil {
			t.Error(err)
		}
	}()

	cancelled := make(chan model.MigrationShadowLifecycleResult, 1)
	go func() {
		cancelled <- f.store.CancelMigrationShadowFacts(context.Background(), source.OperationID, receipt.RequestHash, receipt.CheckpointHash)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel never reached write")
	}

	read := make(chan model.MigrationShadowLifecycleResult, 1)
	go func() { read <- f.store.ReadMigrationShadowLifecycleFacts(context.Background(), source.OperationID) }()

	var app string
	require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&app).Error)
	require.Eventually(t, func() bool {
		var n int64

		return db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).
			Scan(&n).
			Error == nil &&
			n > 0
	}, 3*time.Second, 5*time.Millisecond)
	close(release)

	var result, observed model.MigrationShadowLifecycleResult
	select {
	case result = <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not join")
	}

	select {
	case observed = <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("read did not join")
	}

	require.Equal(t, "ready", result.State)
	require.Equal(t, "cancelled", result.Observation.Lifecycle.State)
	require.Equal(t, "abandoned", result.Observation.PreparedBinding.State)
	// Repeatable-read may reject a control row changed since snapshot creation;
	// a rejected snapshot must discard all partial observations.
	if observed.State == "ready" {
		require.Equal(t, result, observed)
	} else {
		require.NotEqual(t, "absent", observed.State)
		require.Nil(t, observed.Observation)
	}

	require.Equal(
		t,
		result,
		f.store.ReadMigrationShadowLifecycleFacts(context.Background(), source.OperationID),
	)
}
