package model_test

import (
	"context"
	"fmt"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestPostgresMigrationInventoryReadOnly(t *testing.T) {
	db := queueLockPostgres(t)
	f := admissionFixtureOn(t, db, "isolated-inventory-pg")
	inventoryFixture(t, f)
	// Actual server rejects every DML statement against inventory source tables.
	require.NoError(t, db.Exec(`CREATE FUNCTION inventory_no_writes() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'inventory is read only'; END $$`).Error)
	for _, table := range []string{"channels", "model_configs", "channel_admission_controls", "channel_admission_proofs", "channel_admission_barriers", "channel_admission_families", "channel_admission_uses", "channel_migration_protocols", "channel_migration_prepareds", "channel_migration_probe_records"} {
		require.NoError(t, db.Exec(fmt.Sprintf("CREATE TRIGGER inventory_no_writes BEFORE INSERT OR UPDATE OR DELETE ON %s FOR EACH STATEMENT EXECUTE FUNCTION inventory_no_writes()", table)).Error)
	}
	got := f.store.ReadMigrationInventory(context.Background(), nil)
	require.Equal(t, "ready", got.State)
	require.True(t, got.Complete)
	require.Len(t, got.Channels, 5)
}
func TestPostgresMigrationInventoryControlLockSnapshot(t *testing.T) {
	for _, outcome := range []string{"rollback", "commit", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			db := queueLockPostgres(t)
			f := admissionFixtureOn(t, db, "isolated-inventory-pg")
			inventoryFixture(t, f)
			before := f.store.ReadMigrationInventory(context.Background(), nil)
			require.Equal(t, "ready", before.State)
			pool, e := db.DB()
			require.NoError(t, e)
			holder, e := pool.Begin()
			require.NoError(t, e)
			defer holder.Rollback()
			_, e = holder.Exec("UPDATE channel_admission_controls SET revision=revision+1 WHERE id=1")
			require.NoError(t, e)
			_, e = holder.Exec("UPDATE channels SET name='changed-after-wait' WHERE id=1")
			require.NoError(t, e)
			_, e = holder.Exec("INSERT INTO model_configs (model,config,plugin) VALUES ('new-after-wait','{}','{}')")
			require.NoError(t, e)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan model.MigrationInventory, 1)
			go func() { done <- f.store.ReadMigrationInventory(ctx, nil) }()
			var app string
			require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&app).Error)
			require.Eventually(t, func() bool {
				var n int64
				return db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).Scan(&n).Error == nil && n > 0
			}, 3*time.Second, 5*time.Millisecond)
			switch outcome {
			case "commit":
				require.NoError(t, holder.Commit())
			case "rollback":
				require.NoError(t, holder.Rollback())
			case "cancel":
				cancel()
			}
			got := <-done
			if outcome == "rollback" {
				require.Equal(t, before, got)
			} else {
				require.Equal(t, "unsupported", got.State)
				require.False(t, got.Complete)
				require.Empty(t, got.Channels)
			}
			if outcome == "commit" {
				fresh := f.store.ReadMigrationInventory(context.Background(), nil)
				require.Equal(t, "ready", fresh.State)
				require.Equal(t, "1", fresh.Revision)
				require.Len(t, fresh.Configs, 2)
				require.Equal(t, "changed-after-wait", fresh.Channels[0].Routing.Name)
				require.NotEqual(t, before.SourceSnapshotHash, fresh.SourceSnapshotHash)
			}
		})
	}
}
