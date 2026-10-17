package model_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestPostgresMigrationProbeLockAndRollback(t *testing.T) {
	for _, mode := range []string{"live", "config-drift", "owner-disabled", "proof-revoked", "cancel", "write-rollback"} {
		t.Run(mode, func(t *testing.T) {
			db := queueLockPostgres(t)
			f := admissionFixtureOn(t, db, "probe-pg")
			a, r := probeFixture(t, f)
			h, e := f.store.BeginMigrationProbe(t.Context(), a)
			require.NoError(t, e)
			task := probeNativeTask(t, h)
			if mode == "write-rollback" {
				require.NoError(t, db.Exec(`CREATE FUNCTION reject_probe_bind() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.use_id <> '' THEN RAISE EXCEPTION 'synthetic rollback'; END IF; RETURN NEW; END $$`).Error)
				require.NoError(t, db.Exec(`CREATE TRIGGER reject_probe_bind BEFORE UPDATE ON channel_migration_probe_runs FOR EACH ROW EXECUTE FUNCTION reject_probe_bind()`).Error)
			}
			pool, e := db.DB()
			require.NoError(t, e)
			holder, e := pool.Begin()
			require.NoError(t, e)
			defer holder.Rollback()
			_, e = holder.Exec("UPDATE channel_admission_controls SET revision=revision WHERE id=1")
			require.NoError(t, e)
			done := make(chan error, 1)
			go func() { _, _, e := model.ReserveMigrationProbeTask(context.Background(), db, h, task); done <- e }()
			var app string
			require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&app).Error)
			require.Eventually(t, func() bool {
				var n int64
				return db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).Scan(&n).Error == nil && n > 0
			}, 3*time.Second, 5*time.Millisecond)
			switch mode {
			case "config-drift":
				_, e = holder.Exec("UPDATE model_configs SET rpm=99")
			case "owner-disabled":
				_, e = holder.Exec("UPDATE groups SET status=2 WHERE id=$1", a.GroupID)
			case "proof-revoked":
				_, e = holder.Exec("UPDATE channel_admission_proofs SET revoked=true")
			case "cancel":
				_, e = holder.Exec("UPDATE channel_migration_shadow_resources SET lifecycle_state='cancelled',lifecycle_revision=created_revision+1,cancelled_at_ms=created_at_ms+1 WHERE operation_id=$1", r.OperationID)
				if e == nil {
					_, e = holder.Exec("UPDATE channel_admission_controls SET revision=revision+1")
				}
			}
			require.NoError(t, e)
			require.NoError(t, holder.Commit())
			e = <-done
			if mode == "live" {
				require.NoError(t, e)
			} else {
				require.Error(t, e)
			}
			var tasks, uses int64
			require.NoError(t, db.Model(&model.NativeTask{}).Count(&tasks).Error)
			require.NoError(t, db.Model(&model.ChannelAdmissionUse{}).Count(&uses).Error)
			if mode == "live" {
				require.EqualValues(t, 1, tasks)
				require.EqualValues(t, 1, uses)
			} else {
				require.Zero(t, tasks)
				require.Zero(t, uses)
			}
			var run model.ChannelMigrationProbeRun
			require.NoError(t, db.First(&run).Error)
			if mode != "live" {
				require.Empty(t, run.UseID)
				require.Empty(t, run.OwnerHash)
				require.Zero(t, run.BoundAtMs)
			}
		})
	}
}

type probeProcessInput struct {
	Authorization model.MigrationProbeAuthorization
	Proof         model.VerifiedDeploymentProof
	Executor      model.ChannelExecutorIdentity
}

func TestPostgresMigrationProbeProcessChild(t *testing.T) {
	path := os.Getenv("B2_PROBE_PROCESS_INPUT")
	if path == "" {
		t.Skip("local multiprocess child")
	}
	require.True(t, strings.HasPrefix(path, "/private/tmp/") || strings.Contains(path, "/T/"))
	raw, e := os.ReadFile(path)
	require.NoError(t, e)
	var input probeProcessInput
	require.NoError(t, json.Unmarshal(raw, &input))
	dsn := os.Getenv("B2_PROBE_PROCESS_DSN")
	u, e := url.Parse(dsn)
	require.NoError(t, e)
	require.Equal(t, "127.0.0.1", u.Hostname())
	require.Equal(t, "/g3_queue_test", u.Path)
	db, e := model.OpenPostgreSQL(dsn)
	require.NoError(t, e)
	pool, e := db.DB()
	require.NoError(t, e)
	defer pool.Close()
	store := &model.ChannelAdmissionStore{DB: db, LogDB: db, Executor: input.Executor, LoadDeployment: func(context.Context) (model.VerifiedDeploymentProof, error) { return input.Proof, nil }}
	h, e := store.BeginMigrationProbe(t.Context(), input.Authorization)
	require.NoError(t, e)
	_, created, e := model.ReserveMigrationProbeTask(t.Context(), db, h, probeNativeTask(t, h))
	if e == nil {
		require.True(t, created)
		fmt.Println("PROBE_RESERVED_ONCE")
	} else {
		require.ErrorIs(t, e, model.ErrChannelAdmissionConflict)
		fmt.Println("PROBE_ALREADY_BOUND")
	}
}
func TestPostgresMigrationProbeTwoProcessesAndRestart(t *testing.T) {
	db := queueLockPostgres(t)
	f := admissionFixtureOn(t, db, "probe-process")
	a, _ := probeFixture(t, f)
	raw, e := json.Marshal(probeProcessInput{a, f.proof, f.store.Executor})
	require.NoError(t, e)
	dir, e := os.MkdirTemp("/private/tmp", "probe-process-")
	require.NoError(t, e)
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "input.json")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	var schema string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&schema).Error)
	u, e := url.Parse(os.Getenv("B2_G3_TEST_POSTGRES_DSN"))
	require.NoError(t, e)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	binary, e := os.Executable()
	require.NoError(t, e)
	results := make(chan string, 2)
	for i := 0; i < 2; i++ {
		go func() {
			cmd := exec.Command(binary, "-test.run=^TestPostgresMigrationProbeProcessChild$", "-test.v")
			cmd.Env = append(os.Environ(), "B2_PROBE_PROCESS_INPUT="+path, "B2_PROBE_PROCESS_DSN="+u.String())
			output, e := cmd.CombinedOutput()
			if e != nil {
				results <- "FAILED:" + string(output)
			} else {
				results <- string(output)
			}
		}()
	}
	output := <-results
	output += <-results
	require.NotContains(t, output, "FAILED:", output)
	require.Equal(t, 1, strings.Count(output, "PROBE_RESERVED_ONCE"))
	require.Equal(t, 1, strings.Count(output, "PROBE_ALREADY_BOUND"))
	var count int64
	require.NoError(t, db.Model(&model.NativeTask{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	require.NoError(t, db.Model(&model.ChannelAdmissionUse{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	// Losing the resource-run row is not permission to recreate history, even if
	// the native reservation was separately removed after a wallet refusal.
	require.NoError(t, db.Exec("DELETE FROM channel_migration_probe_runs").Error)
	require.NoError(t, db.Exec("DELETE FROM native_tasks").Error)
	_, e = f.store.BeginMigrationProbe(t.Context(), a)
	require.Error(t, e)
	require.NoError(t, db.Model(&model.NativeTask{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestPostgresMigrationProbeDraft(t *testing.T) {
	db := queueLockPostgres(t)
	f := admissionFixtureOn(t, db, "probe-draft-pg")
	a, _ := probeFixture(t, f)
	replayProbeDraft(t, f)
	h, e := f.store.BeginMigrationProbe(t.Context(), a)
	require.NoError(t, e)
	_, _, e = model.ReserveMigrationProbeTask(t.Context(), db, h, probeNativeTask(t, h))
	require.NoError(t, e)
}
