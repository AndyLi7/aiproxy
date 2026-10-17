package model_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func billingContextFixture(t *testing.T, f *admissionFixture) (model.MigrationProbeAuthorization, *model.MigrationShadowReceipt, model.MigrationProbeBillingContextRequest) {
	a, r := probeFixture(t, f)
	h, e := f.store.BeginMigrationProbe(t.Context(), a)
	require.NoError(t, e)
	task := probeNativeTask(t, h)
	_, _, e = model.ReserveMigrationProbeTask(t.Context(), f.store.DB, h, task)
	require.NoError(t, e)
	require.NoError(t, f.store.DB.Model(&model.NativeTask{}).Where("id=?", task.ID).Updates(map[string]any{"upstream_id": "synthetic-billing-upstream", "status": "queued"}).Error)
	return a, r, model.MigrationProbeBillingContextRequest{BillingOperationID: task.BillingOperationID, TaskID: task.ID, GroupID: task.GroupID}
}
func TestMigrationProbeBillingContextActualBindingAndCancelledRead(t *testing.T) {
	f := newAdmissionFixture(t)
	a, r, input := billingContextFixture(t, f)
	f.store.LoadDeployment = nil
	read := f.store.ReadMigrationProbeBillingContext(t.Context(), input)
	require.Equal(t, "ready", read.State, read.Reason)
	require.NotNil(t, read.Context)
	require.False(t, read.Context.BillingSettled)
	require.Equal(t, a.Command.Seal.From, read.Context.From)
	require.Equal(t, a.Command.Seal.To, read.Context.To)
	require.Equal(t, "synthetic-billing-upstream", read.Context.Binding.UpstreamTaskID)
	before := read.Context.TaskBindingHash
	// Neither unrelated source edits nor cancellation rewrite the original paid task.
	require.NoError(t, f.store.DB.Exec("UPDATE model_configs SET rpm=98").Error)
	_, e := f.store.CancelMigrationShadow(t.Context(), a.Command.Seal.OperationID, r.RequestHash, r.CheckpointHash)
	require.NoError(t, e)
	read = f.store.ReadMigrationProbeBillingContext(t.Context(), input)
	require.Equal(t, "ready", read.State, read.Reason)
	require.Equal(t, "cancelled", read.Context.ResourceState)
	require.Equal(t, before, read.Context.TaskBindingHash)
	raw, e := json.Marshal(read)
	require.NoError(t, e)
	for _, secret := range []string{"synthetic-preparation-old", shadowTestKey, "local synthetic probe", `"quoteJson"`, `"input"`, `"ownerHash"`} {
		require.NotContains(t, string(raw), secret)
	}
	_, e = f.store.FinalizeMigrationProbe(t.Context(), a.Command.Seal.OperationID, nil)
	require.Error(t, e)
}
func TestMigrationProbeBillingContextRejectsTornAndWrongBindings(t *testing.T) {
	for _, mode := range []string{"group", "billing", "quote", "task-model", "run-spec", "use", "resource", "task-missing", "run-missing", "partial", "upstream-pending", "settled-not-required"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdmissionFixture(t)
			_, _, input := billingContextFixture(t, f)
			want := "conflict"
			switch mode {
			case "group":
				input.GroupID = "other"
			case "billing":
				input.BillingOperationID = "other"
			case "quote":
				require.NoError(t, f.store.DB.Exec("UPDATE native_tasks SET prepayment_quote_json='{}'").Error)
			case "task-model":
				require.NoError(t, f.store.DB.Exec("UPDATE native_tasks SET model='other'").Error)
			case "run-spec":
				require.NoError(t, f.store.DB.Exec("UPDATE channel_migration_probe_runs SET spec_hash=?", strings.Repeat("0", 64)).Error)
			case "use":
				require.NoError(t, f.store.DB.Exec("UPDATE channel_admission_uses SET key_digest=?", strings.Repeat("0", 64)).Error)
			case "resource":
				require.NoError(t, f.store.DB.Exec("DELETE FROM channel_migration_shadow_resources").Error)
			case "task-missing":
				require.NoError(t, f.store.DB.Exec("DELETE FROM native_tasks").Error)
				want = "unknown"
			case "run-missing":
				require.NoError(t, f.store.DB.Exec("DELETE FROM channel_migration_probe_runs").Error)
				want = "unknown"
			case "partial":
				require.NoError(t, f.store.DB.Exec("ALTER TABLE channel_migration_probe_runs DROP COLUMN spec_hash").Error)
				want = "unavailable"
			case "upstream-pending":
				require.NoError(t, f.store.DB.Exec("UPDATE native_tasks SET upstream_id=''").Error)
				want = "pending"
			case "settled-not-required":
				want = "ready"
			}
			read := f.store.ReadMigrationProbeBillingContext(t.Context(), input)
			require.Equal(t, want, read.State, read.Reason)
			if want != "ready" {
				require.Nil(t, read.Context)
			}
		})
	}
}
func TestMigrationProbeBillingContextAbsentAndReadOnly(t *testing.T) {
	f := newAdmissionFixture(t)
	_, _, input := billingContextFixture(t, f)
	pool, e := f.store.DB.DB()
	require.NoError(t, e)
	pool.SetMaxOpenConns(1)
	require.NoError(t, f.store.DB.Exec("PRAGMA query_only=ON").Error)
	require.Equal(t, "ready", f.store.ReadMigrationProbeBillingContext(t.Context(), input).State)
	ordinary := model.MigrationProbeBillingContextRequest{BillingOperationID: "ordinary-bill", TaskID: "ordinary-task", GroupID: input.GroupID}
	require.Equal(t, "not_probe", f.store.ReadMigrationProbeBillingContext(t.Context(), ordinary).State)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.Equal(t, "unavailable", f.store.ReadMigrationProbeBillingContext(ctx, input).State)
	require.NoError(t, f.store.DB.Exec("PRAGMA query_only=OFF").Error)
	require.NoError(t, f.store.DB.Exec("DROP TABLE native_tasks").Error)
	require.Equal(t, "unavailable", f.store.ReadMigrationProbeBillingContext(t.Context(), ordinary).State)
}
func TestPostgresMigrationProbeBillingContextConsistentNoDML(t *testing.T) {
	for _, mode := range []string{"live", "cancel", "changed-binding", "missing-run"} {
		t.Run(mode, func(t *testing.T) {
			db := queueLockPostgres(t)
			f := admissionFixtureOn(t, db, "billing-context-pg")
			_, _, input := billingContextFixture(t, f)
			pool, e := db.DB()
			require.NoError(t, e)
			holder, e := pool.Begin()
			require.NoError(t, e)
			defer holder.Rollback()
			_, e = holder.Exec("UPDATE channel_admission_controls SET revision=revision WHERE id=1")
			require.NoError(t, e)
			done := make(chan model.MigrationProbeBillingContextResult, 1)
			go func() { done <- f.store.ReadMigrationProbeBillingContext(context.Background(), input) }()
			var app string
			require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&app).Error)
			require.Eventually(t, func() bool {
				var n int64
				return db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).Scan(&n).Error == nil && n > 0
			}, 3*time.Second, 5*time.Millisecond)
			switch mode {
			case "cancel":
				_, e = holder.Exec("UPDATE channel_migration_shadow_resources SET lifecycle_state='cancelled',lifecycle_revision=created_revision+1,cancelled_at_ms=created_at_ms+1")
				require.NoError(t, e)
				_, e = holder.Exec("UPDATE channel_admission_controls SET revision=revision+1")
			case "changed-binding":
				_, e = holder.Exec("UPDATE native_tasks SET upstream_id='other'")
			case "missing-run":
				_, e = holder.Exec("DELETE FROM channel_migration_probe_runs")
			}
			require.NoError(t, e)
			require.NoError(t, holder.Commit())
			first := <-done
			// PostgreSQL may report a repeatable-read serialization failure after the
			// locked control changes. It must never combine old run and new task rows.
			if first.State == "ready" {
				require.Equal(t, "synthetic-billing-upstream", first.Context.Binding.UpstreamTaskID)
			} else {
				require.Equal(t, "unavailable", first.State)
			}
			for _, table := range []string{"channel_admission_controls", "channel_migration_shadow_resources", "channel_migration_probe_runs", "native_tasks", "channel_admission_uses"} {
				require.NoError(t, db.Exec("CREATE OR REPLACE FUNCTION deny_context_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'read performed DML'; END $$").Error)
				require.NoError(t, db.Exec("CREATE TRIGGER deny_context_write BEFORE INSERT OR UPDATE OR DELETE ON "+table+" FOR EACH STATEMENT EXECUTE FUNCTION deny_context_write()").Error)
			}
			read := f.store.ReadMigrationProbeBillingContext(t.Context(), input)
			if mode == "missing-run" {
				require.Equal(t, "unknown", read.State)
			} else {
				require.Equal(t, "ready", read.State, read.Reason)
				if mode == "cancel" {
					require.Equal(t, "cancelled", read.Context.ResourceState)
				}
				if mode == "changed-binding" {
					require.Equal(t, "other", read.Context.Binding.UpstreamTaskID)
				}
			}
		})
	}
}
