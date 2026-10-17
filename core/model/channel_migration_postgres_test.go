package model_test

import (
	"context"
	"database/sql"
	"errors"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
	"time"
)

func TestPostgresPreparedMigrationChannelLock(t *testing.T) {
	for _, stage := range []string{"prepare", "seal", "commit"} {
		for _, outcome := range []string{"live", "deployment-expired", "probe-expired", "revoked", "probe-revoked", "projection-changed", "cancelled"} {
			t.Run(stage+"/"+outcome, func(t *testing.T) {
				db := queueLockPostgres(t)
				f := admissionFixtureOn(t, db, "")
				req, probe := migrationFixture(t, f, 3)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				deadline := time.Now().Add(600 * time.Millisecond)
				if outcome == "deployment-expired" {
					f.proof.ValidUntilMs = deadline.UnixMilli()
					f.refreshSyntheticProof(t)
				}
				if outcome == "probe-expired" {
					probe.ValidUntilMs = deadline.UnixMilli()
					hash, err := probe.Fingerprint()
					require.NoError(t, err)
					require.NoError(t, db.Model(&model.ChannelMigrationProbeRecord{}).Where("evidence_hash = ?", probe.EvidenceHash).Update("fact_hash", hash).Error)
				}
				var revision int64
				if stage != "prepare" {
					_, err := f.store.PrepareMigration(ctx, req)
					require.NoError(t, err)
				}
				if stage == "commit" {
					seal, err := f.store.SealPrepared(ctx, req)
					require.NoError(t, err)
					revision = seal.Revision
				}
				pool, err := db.DB()
				require.NoError(t, err)
				holder, err := pool.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer holder.Rollback()
				var id int
				require.NoError(t, holder.QueryRowContext(ctx, "SELECT id FROM channels WHERE id=3 FOR UPDATE").Scan(&id))
				before := migrationConfig(t, db)
				done := make(chan error, 1)
				go func() {
					var e error
					switch stage {
					case "prepare":
						_, e = f.store.PrepareMigration(ctx, req)
					case "seal":
						_, e = f.store.SealPrepared(ctx, req)
					case "commit":
						_, e = f.store.CommitPreparedPins(ctx, req, revision)
					}
					done <- e
				}()
				var application string
				require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&application).Error)
				require.Eventually(t, func() bool {
					var n int64
					err := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", application).Scan(&n).Error
					return err == nil && n > 0
				}, 2*time.Second, 5*time.Millisecond)
				switch outcome {
				case "deployment-expired", "probe-expired":
					if delay := time.Until(deadline.Add(50 * time.Millisecond)); delay > 0 {
						time.Sleep(delay)
					}
				case "revoked":
					require.NoError(t, db.Model(&model.ChannelAdmissionProof{}).Where("id = ?", f.proof.ProofID).Update("revoked", true).Error)
				case "probe-revoked":
					require.NoError(t, db.Model(&model.ChannelMigrationProbeRecord{}).Where("evidence_hash = ?", probe.EvidenceHash).Update("revoked", true).Error)
				case "projection-changed":
					_, err = holder.ExecContext(ctx, "UPDATE channels SET priority=37 WHERE id=3")
					require.NoError(t, err)
				case "cancelled":
					cancel()
					require.Error(t, <-done)
					rollbackErr := holder.Rollback()
					require.True(t, rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone))
					require.Equal(t, before, migrationConfig(t, db))
					return
				}
				require.NoError(t, holder.Commit())
				err = <-done
				if outcome == "live" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				if outcome == "live" && stage == "commit" {
					require.Equal(t, model.ChannelStatusEnabled, migrationStatus(t, db, 3))
					read, err := f.store.ReadPreparedMigration(context.Background(), req.Seal.OperationID)
					require.NoError(t, err)
					require.Equal(t, "committed", read.State)
				} else {
					require.Equal(t, model.ChannelStatusDisabled, migrationStatus(t, db, 3))
					require.Equal(t, before, migrationConfig(t, db))
				}
			})
		}
	}
}

// A failed last row must roll back both earlier pin writes and channel enabling.
func TestPostgresPreparedMigrationFinalWriteAndMembership(t *testing.T) {
	for _, scenario := range []string{"late-proof-expiry", "final-control-probe-expiry", "write-failure", "new-pin", "replay"} {
		t.Run(scenario, func(t *testing.T) {
			db := queueLockPostgres(t)
			f := admissionFixtureOn(t, db, "")
			req, probe := migrationFixture(t, f, 3)
			ctx := context.Background()
			if scenario == "late-proof-expiry" || scenario == "final-control-probe-expiry" {
				probe.ValidUntilMs = time.Now().Add(time.Second).UnixMilli()
				hash, err := probe.Fingerprint()
				require.NoError(t, err)
				require.NoError(t, db.Model(&model.ChannelMigrationProbeRecord{}).Where("evidence_hash = ?", probe.EvidenceHash).Update("fact_hash", hash).Error)
			}
			_, err := f.store.PrepareMigration(ctx, req)
			require.NoError(t, err)
			seal, err := f.store.SealPrepared(ctx, req)
			require.NoError(t, err)
			before := migrationConfig(t, db)
			switch scenario {
			case "late-proof-expiry":
				require.NoError(t, db.Exec(`CREATE FUNCTION c2_delay() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(1.1); RETURN NEW; END $$`).Error)
				require.NoError(t, db.Exec(`CREATE TRIGGER c2_delay_trigger BEFORE UPDATE ON channel_migration_prepareds FOR EACH ROW EXECUTE FUNCTION c2_delay()`).Error)
			case "final-control-probe-expiry":
				require.NoError(t, db.Exec(`CREATE FUNCTION c2_control_delay() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(1.1); RETURN NEW; END $$`).Error)
				require.NoError(t, db.Exec(`CREATE TRIGGER c2_control_delay_trigger BEFORE UPDATE ON channel_admission_controls FOR EACH ROW WHEN (NEW.revision <> OLD.revision) EXECUTE FUNCTION c2_control_delay()`).Error)
			case "write-failure":
				require.NoError(t, db.Exec(`CREATE FUNCTION c2_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic local rollback'; END $$`).Error)
				require.NoError(t, db.Exec(`CREATE TRIGGER c2_fail_trigger BEFORE UPDATE ON channel_migration_prepareds FOR EACH ROW EXECUTE FUNCTION c2_fail()`).Error)
			case "new-pin":
				require.NoError(t, db.Exec("INSERT INTO model_configs (model,config) SELECT ?,config FROM model_configs WHERE model=?", "late-member", "example").Error)
			}
			result, err := f.store.CommitPreparedPins(ctx, req, seal.Revision)
			if scenario == "replay" {
				require.NoError(t, err)
				again, err := f.store.CommitPreparedPins(ctx, req, seal.Revision)
				require.NoError(t, err)
				require.Equal(t, result, again)
			} else {
				require.Error(t, err)
				require.Equal(t, model.ChannelStatusDisabled, migrationStatus(t, db, 3))
				require.Equal(t, before, migrationConfig(t, db))
				var row model.ChannelMigrationPrepared
				require.NoError(t, db.First(&row).Error)
				require.Equal(t, "sealed", row.State)
			}
		})
	}
}

func TestPostgresPreparedMigrationDraftAndSerializedInsert(t *testing.T) {
	db := queueLockPostgres(t)
	f := admissionFixtureOn(t, db, "")
	script, err := os.ReadFile("../migrations/channel-migration-prepared-v1/postgres.sql")
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	conn, err := pool.Conn(context.Background())
	require.NoError(t, err)
	defer conn.Close()
	for range 2 {
		require.NoError(t, conn.Raw(func(c any) error {
			_, err := c.(*stdlib.Conn).Conn().PgConn().Exec(context.Background(), string(script)).ReadAll()
			return err
		}))
	}
	require.NoError(t, db.Exec("DELETE FROM channel_migration_protocols").Error) // fixture installs its explicit protocol record
	req, _ := migrationFixture(t, f, 3)
	_, err = f.store.PrepareMigration(context.Background(), req)
	require.NoError(t, err)
	seal, err := f.store.SealPrepared(context.Background(), req)
	require.NoError(t, err)
	oldDB, oldLog := model.DB, model.LogDB
	model.DB, model.LogDB = db, db
	t.Cleanup(func() { model.DB, model.LogDB = oldDB, oldLog })
	holder, err := pool.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer holder.Rollback()
	var id int
	require.NoError(t, holder.QueryRow("SELECT id FROM channel_admission_controls WHERE id=1 FOR UPDATE").Scan(&id))
	done := make(chan error, 1)
	go func() { done <- model.SaveModelConfig(model.ModelConfig{Model: "concurrent-unrelated"}) }()
	var application string
	require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&application).Error)
	require.Eventually(t, func() bool {
		var n int64
		err := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", application).Scan(&n).Error
		return err == nil && n > 0
	}, 2*time.Second, 5*time.Millisecond)
	var n int64
	require.NoError(t, db.Table("model_configs").Where("model = ?", "concurrent-unrelated").Count(&n).Error)
	require.Zero(t, n)
	require.NoError(t, holder.Commit())
	require.NoError(t, <-done)
	_, err = f.store.CommitPreparedPins(context.Background(), req, seal.Revision)
	require.Error(t, err)
	require.Equal(t, model.ChannelStatusDisabled, migrationStatus(t, db, 3))
}
