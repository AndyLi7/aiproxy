package controller

import (
	"context"
	"fmt"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func retiredShortProof(t *testing.T, f *probeHTTPFixture) time.Time {
	t.Helper()
	proof, e := f.store.LoadDeployment(t.Context())
	require.NoError(t, e)
	deadline := time.Now().Add(4 * time.Second)
	proof.ValidUntilMs = deadline.UnixMilli()
	proof.RevocationFreshUntilMs = proof.ValidUntilMs
	h, e := proof.Fingerprint()
	require.NoError(t, e)
	require.NoError(t, f.db.Model(&model.ChannelAdmissionProof{}).Where("id=?", proof.ProofID).UpdateColumn("fact_hash", h).Error)
	f.store.LoadDeployment = func(context.Context) (model.VerifiedDeploymentProof, error) { return proof, nil }
	restore := model.ConfigureChannelAdmissionRuntime(f.store.Executor, f.store.LoadDeployment)
	t.Cleanup(restore)
	return deadline
}
func TestPostgresMigrationRetiredHTTPFinalWrite(t *testing.T) {
	for _, operation := range []string{"create", "cutover"} {
		for _, mode := range []string{"live", "expired"} {
			t.Run(operation+"-"+mode, func(t *testing.T) {
				f, q := retiredHTTPFixtureFrom(t, newProbeHTTPFixtureDB(t, cutoverHTTPPostgres(t)))
				var binding model.MigrationProbeCutoverBinding
				if operation == "cutover" {
					binding, _ = prepareRetiredHTTP(t, f, q)
				}
				var deadline time.Time
				if mode == "expired" {
					deadline = retiredShortProof(t, f)
				}
				table := "channel_admission_controls"
				if operation == "cutover" {
					table = "model_configs"
				}
				var before int64
				require.NoError(t, f.db.Raw("SELECT revision FROM channel_admission_controls WHERE id=1").Scan(&before).Error)
				require.NoError(t, f.db.Exec(`CREATE FUNCTION reverse_http_pause() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(8731077); RETURN NEW; END $$`).Error)
				require.NoError(t, f.db.Exec("CREATE TRIGGER reverse_http_pause BEFORE UPDATE ON "+table+" FOR EACH ROW EXECUTE FUNCTION reverse_http_pause()").Error)
				pool, e := f.db.DB()
				require.NoError(t, e)
				holder, e := pool.BeginTx(t.Context(), nil)
				require.NoError(t, e)
				defer holder.Rollback()
				_, e = holder.Exec("SELECT pg_advisory_xact_lock(8731077)")
				require.NoError(t, e)
				done := make(chan string, 1)
				go func() {
					path := "/api/channel-migrations/operations/" + q.Command.Seal.OperationID
					if operation == "create" {
						_, raw := f.call(t, "POST", path+"/retired-target", q, config.AdminKey)
						done <- string(raw)
					} else {
						_, raw := f.call(t, "POST", path+"/cutover", binding, config.AdminKey)
						done <- string(raw)
					}
				}()
				var app string
				require.NoError(t, f.db.Raw("SELECT current_setting('application_name')").Scan(&app).Error)
				require.Eventually(t, func() bool {
					var n int64
					e := f.db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock' AND query ILIKE ?", app, "%UPDATE%"+table+"%").Scan(&n).Error
					return e == nil && n > 0
				}, 2*time.Second, 10*time.Millisecond)
				var middle int64
				require.NoError(t, f.db.Raw("SELECT revision FROM channel_admission_controls WHERE id=1").Scan(&middle).Error)
				require.Equal(t, before, middle)
				if mode == "expired" {
					if wait := time.Until(deadline.Add(50 * time.Millisecond)); wait > 0 {
						time.Sleep(wait)
					}
				}
				require.NoError(t, holder.Commit())
				select {
				case raw := <-done:
					if mode == "live" {
						require.Contains(t, raw, `"state":"ready"`)
						if operation == "cutover" {
							require.Contains(t, raw, `"state":"committed"`)
						}
					} else {
						require.NotContains(t, raw, `"state":"ready"`)
						var after int64
						require.NoError(t, f.db.Raw("SELECT revision FROM channel_admission_controls WHERE id=1").Scan(&after).Error)
						require.Equal(t, before, after)
						if operation == "create" {
							var n int64
							require.NoError(t, f.db.Model(&model.ChannelMigrationRetiredTargetResource{}).Count(&n).Error)
							require.Zero(t, n)
						} else {
							var p model.ChannelMigrationPrepared
							require.NoError(t, f.db.First(&p, "operation_id=?", q.Command.Seal.OperationID).Error)
							require.Equal(t, "prepared", p.State)
							var n int64
							require.NoError(t, f.db.Model(&model.ChannelAdmissionBarrier{}).Where("operation_id=?", q.Command.Seal.OperationID).Count(&n).Error)
							require.Zero(t, n)
						}
					}
				case <-time.After(5 * time.Second):
					t.Fatal("reverse HTTP did not exit")
				}
				var channels []model.Channel
				require.NoError(t, f.db.Find(&channels).Error)
				require.Len(t, channels, 2)
				for _, c := range channels {
					require.Equal(t, model.ChannelStatusEnabled, c.Status)
				}
			})
		}
	}
}
func TestPostgresMigrationRetiredHTTPReadNoDML(t *testing.T) {
	f, q := retiredHTTPFixtureFrom(t, newProbeHTTPFixtureDB(t, cutoverHTTPPostgres(t)))
	created := retiredHTTPCall(t, f, q, "", q)
	require.Equal(t, "ready", created.State)
	require.NoError(t, f.db.Exec(`CREATE FUNCTION reverse_read_no_dml() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'read cannot write'; END $$`).Error)
	var tables []string
	require.NoError(t, f.db.Raw("SELECT tablename FROM pg_tables WHERE schemaname=current_schema()").Scan(&tables).Error)
	for i, table := range tables {
		require.NoError(t, f.db.Exec(fmt.Sprintf("CREATE TRIGGER reverse_read_no_dml_%d BEFORE INSERT OR UPDATE OR DELETE ON %s FOR EACH STATEMENT EXECUTE FUNCTION reverse_read_no_dml()", i, table)).Error)
	}
	require.Error(t, f.db.Exec("UPDATE channel_admission_controls SET revision=revision").Error)
	require.Equal(t, created, retiredHTTPCall(t, f, q, "/read", retiredReadBody(q)))
	changed := q
	changed.Command.ExpectedPinHash = strings.Repeat("0", 64)
	require.Equal(t, "conflict", retiredHTTPCall(t, f, changed, "/read", retiredReadBody(changed)).State)
}
