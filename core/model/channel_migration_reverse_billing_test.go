package model_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func reverseBillingFixture(
	t *testing.T,
	f *admissionFixture,
) (model.MigrationRetiredTargetRequest, *model.ChannelMigrationRetiredTargetResource, model.MigrationProbeBillingContextRequest) {
	t.Helper()
	a, q := reverseFixture(t, f)
	r, e := f.store.PrepareRetiredTargetResource(t.Context(), q)
	require.NoError(t, e)

	a.Command = q.Command
	a.ResourceRequestHash = r.RequestHash
	a.CheckpointHash = r.CheckpointHash
	h, e := f.store.BeginMigrationProbe(t.Context(), a)
	require.NoError(t, e)
	task := probeNativeTask(t, h)
	_, _, e = model.ReserveMigrationProbeTask(t.Context(), f.store.DB, h, task)
	require.NoError(t, e)
	require.NoError(
		t,
		f.store.DB.Model(&model.NativeTask{}).
			Where("id=?", task.ID).
			Updates(map[string]any{"upstream_id": "reverse-billing-original", "status": "queued"}).
			Error,
	)

	return q, r, model.MigrationProbeBillingContextRequest{
		BillingOperationID: task.BillingOperationID,
		TaskID:             task.ID,
		GroupID:            task.GroupID,
	}
}

func TestReverseBillingContextOriginalRead(t *testing.T) {
	f := newAdmissionFixture(t)
	q, r, input := reverseBillingFixture(t, f)
	f.store.LoadDeployment = nil
	read := f.store.ReadMigrationProbeBillingContext(t.Context(), input)
	require.Equal(t, "ready", read.State, read.Reason)
	require.Equal(t, "reverse-billing-original", read.Context.Binding.UpstreamTaskID)
	require.Equal(t, model.MigrationReuseRetiredTarget, read.Context.TargetResourceKind)
	require.Equal(t, q.Command.Seal.RevertOf, read.Context.RevertOf)
	original := *read.Context
	_, e := f.store.CancelRetiredTargetResource(
		t.Context(),
		r.OperationID,
		r.RequestHash,
		r.CheckpointHash,
	)
	require.NoError(t, e)
	pool, e := f.store.DB.DB()
	require.NoError(t, e)
	pool.SetMaxOpenConns(1)
	require.NoError(t, f.store.DB.Exec("PRAGMA query_only=ON").Error)
	read = f.store.ReadMigrationProbeBillingContext(t.Context(), input)
	require.Equal(t, "ready", read.State, read.Reason)
	require.Equal(t, "cancelled", read.Context.ResourceState)
	original.ResourceState = "cancelled"
	require.Equal(t, original, *read.Context)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.Equal(t, "unavailable", f.store.ReadMigrationProbeBillingContext(ctx, input).State)
	require.NoError(t, f.store.DB.Exec("PRAGMA query_only=OFF").Error)
	_, e = f.store.PrepareReverseMigrationFromProbe(t.Context(), r.OperationID, nil)
	require.Error(t, e)
}

func TestReverseBillingContextHistoryDoesNotRequireCurrentHead(t *testing.T) {
	f := newAdmissionFixture(t)
	q, _, input := reverseBillingFixture(t, f)
	original := f.store.ReadMigrationProbeBillingContext(t.Context(), input)
	require.Equal(t, "ready", original.State, original.Reason)

	for i := range 3 {
		reverseSyntheticProbe(t, f, q.Command)
		_, e := f.store.PrepareReverseMigration(t.Context(), q.Command)
		require.NoError(t, e)
		_, e = f.store.CutoverReversePrepared(t.Context(), q.Command)
		require.NoError(t, e)
		read := f.store.ReadMigrationProbeBillingContext(t.Context(), input)
		require.Equal(t, "ready", read.State, read.Reason)
		require.Equal(t, original.Context.Binding, read.Context.Binding)
		require.Equal(t, original.Context.SpecHash, read.Context.SpecHash)
		require.Equal(t, original.Context.TaskBindingHash, read.Context.TaskBindingHash)

		if i < 2 {
			q = reverseRequest(t, f, q.Command, fmt.Sprintf("later-reverse-%d", i))
			_, e = f.store.PrepareRetiredTargetResource(t.Context(), q)
			require.NoError(t, e)
		}
	}
	// The stored original spec is sufficient for reading this historical bill.
	// A new attempt would separately need current projection/review authorization.
	require.NoError(t, f.store.DB.Exec("UPDATE channels SET model_mapping='{}'").Error)
	require.NoError(t, f.store.DB.Exec("UPDATE model_configs SET config='{}'").Error)
	read := f.store.ReadMigrationProbeBillingContext(t.Context(), input)
	require.Equal(t, "ready", read.State, read.Reason)
	require.Equal(t, original.Context.Binding, read.Context.Binding)
}

func TestReverseBillingContextRejectsBrokenBindings(t *testing.T) {
	for _, mode := range []string{"group", "billing", "quote", "task", "use", "spec", "kind", "revert", "credential", "missing-resource", "missing-schema", "partial-schema", "version", "checkpoint", "retirement", "historical-prepared", "dual-resource", "run-missing", "task-missing", "upstream-pending", "cancelled-with-effects"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdmissionFixture(t)
			q, r, input := reverseBillingFixture(t, f)
			db := f.store.DB

			want := "conflict"
			switch mode {
			case "group":
				input.GroupID = "wrong"
			case "billing":
				input.BillingOperationID = "wrong"
			case "quote":
				require.NoError(
					t,
					db.Exec("UPDATE native_tasks SET prepayment_quote_json='{}'").Error,
				)
			case "task":
				require.NoError(
					t,
					db.Exec("UPDATE native_tasks SET credential_scope='wrong'").Error,
				)
			case "use":
				require.NoError(
					t,
					db.Exec(
						"UPDATE channel_admission_uses SET key_digest=?",
						strings.Repeat("0", 64),
					).Error,
				)
			case "spec":
				require.NoError(
					t,
					db.Exec(
						"UPDATE channel_migration_probe_runs SET spec_hash=?",
						strings.Repeat("0", 64),
					).Error,
				)
			case "kind", "revert", "credential":
				// Simulate internally inconsistent durable evidence even when its JSON hashes agree.
				var run model.ChannelMigrationProbeRun
				require.NoError(t, db.First(&run, "operation_id=?", r.OperationID).Error)

				var a model.MigrationProbeAuthorization
				require.NoError(t, json.Unmarshal([]byte(run.AuthorizationJSON), &a))

				if mode == "kind" {
					a.Command.TargetResourceKind = ""
					a.Command.Seal.RevertOf = ""
				}

				if mode == "revert" {
					a.Command.Seal.RevertOf = "wrong-retirement"
				}

				if mode == "credential" {
					a.Command.FromCredentialID = "wrong-credential"
				}

				raw, _ := json.Marshal(a)
				cmd, _ := json.Marshal(a.Command)
				ah := sha256.Sum256(raw)
				ch := sha256.Sum256(cmd)
				require.NoError(
					t,
					db.Model(&run).
						Updates(map[string]any{"authorization_json": string(raw), "authorization_hash": hex.EncodeToString(ah[:]), "command_hash": hex.EncodeToString(ch[:])}).
						Error,
				)
			case "missing-resource":
				require.NoError(t, db.Delete(r).Error)
			case "missing-schema":
				require.NoError(
					t,
					db.Migrator().DropTable(&model.ChannelMigrationRetiredTargetResource{}),
				)
			case "partial-schema":
				require.NoError(
					t,
					db.Exec(
						"ALTER TABLE channel_migration_retired_target_resources DROP COLUMN retirement_request_hash",
					).Error,
				)
				require.False(
					t,
					db.Migrator().
						HasColumn(&model.ChannelMigrationRetiredTargetResource{}, "retirement_request_hash"),
				)

				want = "unavailable"
			case "version":
				require.NoError(t, db.Model(r).UpdateColumn("version", 99).Error)

				want = "unavailable"
			case "checkpoint":
				require.NoError(
					t,
					db.Model(r).UpdateColumn("checkpoint_hash", strings.Repeat("0", 64)).Error,
				)
			case "retirement":
				require.NoError(
					t,
					db.Exec(
						"UPDATE channel_admission_barriers SET request_hash=? WHERE operation_id=?",
						strings.Repeat("0", 64),
						q.Command.Seal.RevertOf,
					).Error,
				)
			case "historical-prepared":
				require.NoError(
					t,
					db.Exec(
						"DELETE FROM channel_migration_prepareds WHERE operation_id=?",
						q.Command.Seal.RevertOf,
					).Error,
				)
			case "dual-resource":
				// An inconsistent duplicate shadow must not permit a cross-kind fallback.
				var old model.ChannelMigrationShadowResource
				require.NoError(t, db.First(&old).Error)

				var req model.MigrationShadowRequest
				require.NoError(t, json.Unmarshal([]byte(old.RequestJSON), &req))
				req.OperationID = r.OperationID
				raw, _ := json.Marshal(req)
				// The loader's validation rejects inconsistent identity too; no cross-kind fallback.
				require.NoError(
					t,
					db.Model(&old).
						Updates(map[string]any{"operation_id": r.OperationID, "request_json": string(raw)}).
						Error,
				)
			case "run-missing":
				require.NoError(t, db.Exec("DELETE FROM channel_migration_probe_runs").Error)

				want = "unknown"
			case "task-missing":
				require.NoError(t, db.Exec("DELETE FROM native_tasks").Error)

				want = "unknown"
			case "upstream-pending":
				require.NoError(t, db.Exec("UPDATE native_tasks SET upstream_id=''").Error)

				want = "pending"
			case "cancelled-with-effects":
				_, e := f.store.CancelRetiredTargetResource(
					t.Context(),
					r.OperationID,
					r.RequestHash,
					r.CheckpointHash,
				)
				require.NoError(t, e)
				require.NoError(
					t,
					db.Exec(
						"UPDATE channel_admission_barriers SET operation_id=? WHERE operation_id=?",
						r.OperationID,
						q.Command.Seal.RevertOf,
					).Error,
				)
			}

			read := f.store.ReadMigrationProbeBillingContext(t.Context(), input)
			require.Equal(t, want, read.State, read.Reason)
			require.Nil(t, read.Context)
		})
	}
}

func TestReverseBillingContextForwardWireCompatibility(t *testing.T) {
	f := newAdmissionFixture(t)
	_, _, input := billingContextFixture(t, f)
	read := f.store.ReadMigrationProbeBillingContext(t.Context(), input)
	require.Equal(t, "ready", read.State)
	raw, e := json.Marshal(read)
	require.NoError(t, e)
	require.NotContains(t, string(raw), "targetResourceKind")
	require.NotContains(t, string(raw), "revertOf")
	require.Empty(t, read.Context.TargetResourceKind)
	require.Empty(t, read.Context.RevertOf)
}

func TestPostgresReverseBillingContextConsistentNoDML(t *testing.T) {
	for _, mode := range []string{"live", "cancel", "changed-binding", "missing-run", "retirement-corrupt"} {
		t.Run(mode, func(t *testing.T) {
			db := queueLockPostgres(t)
			f := admissionFixtureOn(t, db, "billing-context-pg")
			q, _, input := reverseBillingFixture(t, f)
			pool, e := db.DB()
			require.NoError(t, e)
			holder, e := pool.BeginTx(t.Context(), nil)
			require.NoError(t, e)

			defer func() {
				if err := holder.
					Rollback(); err !=
					nil && !errors.
					Is(err, sql.ErrTxDone) {
					t.Error(err)
				}
			}()

			_, e = holder.ExecContext(t.Context(),

				"UPDATE channel_admission_controls SET revision=revision WHERE id=1")
			require.NoError(t, e)

			done := make(chan model.MigrationProbeBillingContextResult, 1)
			go func() { done <- f.store.ReadMigrationProbeBillingContext(context.Background(), input) }()

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
			case "cancel":
				_, e = holder.ExecContext(
					t.Context(),
					"UPDATE channel_migration_retired_target_resources SET lifecycle_state='cancelled',lifecycle_revision=created_revision+1,cancelled_at_ms=created_at_ms+1",
				)
				require.NoError(t, e)
				_, e = holder.ExecContext(t.Context(),

					"UPDATE channel_admission_controls SET revision=revision+1")
			case "changed-binding":
				_, e = holder.ExecContext(t.Context(),

					"UPDATE native_tasks SET upstream_id='other'")
			case "retirement-corrupt":
				_, e = holder.ExecContext(t.Context(),

					"UPDATE channel_admission_barriers SET request_hash=$1 WHERE operation_id=$2",
					strings.Repeat("0", 64),
					q.Command.Seal.RevertOf,
				)
			case "missing-run":
				_, e = holder.ExecContext(t.Context(),

					"DELETE FROM channel_migration_probe_runs")
			}

			require.NoError(t, e)
			require.NoError(t, holder.Commit())

			first := <-done
			// PostgreSQL may report a repeatable-read serialization failure after the
			// locked control changes. It must never combine old run and new task rows.
			if first.State == "ready" {
				require.Equal(t, "reverse-billing-original", first.Context.Binding.UpstreamTaskID)
			} else {
				require.Equal(t, "unavailable", first.State)
			}

			for _, table := range []string{"channel_admission_controls", "channel_migration_shadow_resources", "channel_migration_retired_target_resources", "channel_migration_prepareds", "channel_admission_barriers", "channel_migration_probe_runs", "native_tasks", "channel_admission_uses"} {
				require.NoError(
					t,
					db.Exec(
						"CREATE OR REPLACE FUNCTION deny_context_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'read performed DML'; END $$",
					).Error,
				)
				require.NoError(
					t,
					db.Exec(
						"CREATE TRIGGER deny_context_write BEFORE INSERT OR UPDATE OR DELETE ON "+table+" FOR EACH STATEMENT EXECUTE FUNCTION deny_context_write()",
					).Error,
				)
			}

			read := f.store.ReadMigrationProbeBillingContext(t.Context(), input)
			switch mode {
			case "retirement-corrupt":
				require.Equal(t, "conflict", read.State)
				require.Nil(t, read.Context)
			case "missing-run":
				require.Equal(t, "unknown", read.State)
			default:
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
