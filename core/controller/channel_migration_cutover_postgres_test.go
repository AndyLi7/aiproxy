//nolint:testpackage // White-box coverage needs the package-private newProbeHTTPFixtureDB fixture or implementation.
package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func cutoverHTTPPostgres(t *testing.T) *gorm.DB {
	t.Helper()

	dsn := os.Getenv("B2_G3_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires disposable local PostgreSQL")
	}

	u, e := url.Parse(dsn)
	require.NoError(t, e)
	require.Contains(t, []string{"127.0.0.1", "localhost", "::1"}, u.Hostname())
	require.Equal(t, "/g3_queue_test", u.Path)

	admin, e := model.OpenPostgreSQL(dsn)
	require.NoError(t, e)

	schema := fmt.Sprintf("cutover_http_%d", time.Now().UnixNano())
	require.NoError(t, admin.Exec("CREATE SCHEMA "+schema).Error)

	q := u.Query()
	q.Set("search_path", schema)
	q.Set("application_name", schema)
	u.RawQuery = q.Encode()
	db, e := model.OpenPostgreSQL(u.String())
	require.NoError(t, e)
	t.Cleanup(func() {
		require.NoError(t, admin.Exec("DROP SCHEMA "+schema+" CASCADE").Error)
		pool, e := admin.DB()
		require.NoError(t, e)
		require.NoError(t, pool.Close())
	})

	return db
}

func TestPostgresMigrationCutoverHTTPAtomicWait(t *testing.T) {
	for _, mode := range []string{"live", "task-changed", "evidence-changed", "final-clock-expired"} {
		t.Run(mode, func(t *testing.T) {
			f := newProbeHTTPFixtureDB(t, cutoverHTTPPostgres(t))

			var deadline time.Time
			if mode == "final-clock-expired" {
				proof, e := f.store.LoadDeployment(t.Context())
				require.NoError(t, e)

				deadline = time.Now().Add(3 * time.Second)
				proof.ValidUntilMs = deadline.UnixMilli()
				proof.RevocationFreshUntilMs = proof.ValidUntilMs
				h, e := proof.Fingerprint()
				require.NoError(t, e)
				require.NoError(
					t,
					f.db.Model(&model.ChannelAdmissionProof{}).
						Where("id=?", proof.ProofID).
						UpdateColumn("fact_hash", h).
						Error,
				)
				f.store.LoadDeployment = func(context.Context) (model.VerifiedDeploymentProof, error) { return proof, nil }

				restore := model.ConfigureChannelAdmissionRuntime(
					f.store.Executor,
					f.store.LoadDeployment,
				)
				defer restore()
			}

			b := prepareHTTPProbeForCutover(t, f)
			pool, e := f.db.DB()
			require.NoError(t, e)
			holder, e := pool.BeginTx(t.Context(), nil)
			require.NoError(t, e)

			defer func() {
				if err := holder.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
					t.Error(err)
				}
			}()

			var (
				id    string
				match string
			)
			switch mode {
			case "task-changed":
				require.NoError(
					t,
					holder.QueryRowContext(t.Context(), "SELECT id FROM native_tasks FOR UPDATE").
						Scan(&id),
				)

				match = "%native_tasks%FOR UPDATE%"
			case "evidence-changed":
				require.NoError(
					t,
					holder.QueryRowContext(t.Context(), "SELECT operation_id FROM channel_migration_probe_runs FOR UPDATE").
						Scan(&id),
				)

				match = "%channel_migration_probe_runs%FOR UPDATE%"
			default:
				require.NoError(
					t,
					f.db.Exec(
						`CREATE FUNCTION pause_final_pin_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(8731042); RETURN NEW; END $$`,
					).Error,
				)
				require.NoError(
					t,
					f.db.Exec(
						`CREATE TRIGGER pause_final_pin BEFORE UPDATE ON model_configs FOR EACH ROW EXECUTE FUNCTION pause_final_pin_write()`,
					).Error,
				)
				_, e = holder.ExecContext(t.Context(), "SELECT pg_advisory_xact_lock(8731042)")
				require.NoError(t, e)

				match = "%UPDATE%model_configs%"
			}

			done := make(chan string, 1)
			go func() {
				_, raw := f.call(
					t,
					"POST",
					"/api/channel-migrations/operations/http-probe-op/cutover",
					b,
					config.AdminKey,
				)
				done <- string(raw)
			}()

			var app string
			require.NoError(
				t,
				f.db.Raw("SELECT current_setting('application_name')").Scan(&app).Error,
			)
			require.Eventually(t, func() bool {
				var n int64

				e := f.db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock' AND query ILIKE ?", app, match).
					Scan(&n).
					Error

				return e == nil && n > 0
			}, 2*time.Second, 10*time.Millisecond)
			// Even the final pin write wait occurs inside one private transaction.
			assertCutoverNotApplied(t, f)

			switch mode {
			case "task-changed":
				_, e = holder.ExecContext(t.Context(),
					"UPDATE native_tasks SET upstream_id='changed-under-lock' WHERE id=$1",
					id,
				)
			case "evidence-changed":
				_, e = holder.ExecContext(
					t.Context(),
					"UPDATE channel_migration_probe_runs SET evidence_task_hash=$1 WHERE operation_id=$2",
					strings.Repeat("0", 64),
					id,
				)
			case "final-clock-expired":
				if wait := time.Until(deadline.Add(50 * time.Millisecond)); wait > 0 {
					time.Sleep(wait)
				}
			}

			require.NoError(t, e)
			require.NoError(t, holder.Commit())

			select {
			case raw := <-done:
				if mode == "live" {
					require.Contains(t, raw, `"state":"committed"`)
				} else {
					require.NotContains(t, raw, `"state":"committed"`)
					assertCutoverNotApplied(t, f)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("HTTP cutover did not exit")
			}

			require.Equal(t, 1, f.providerCalls)
		})
	}
}
