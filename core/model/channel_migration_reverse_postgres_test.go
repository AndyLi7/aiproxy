package model_test

import (
	"context"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestPostgresReverseCutoverFinalWrite(t *testing.T) {
	for _, mode := range []string{"live", "deployment-expired", "freshness-expired", "probe-expired", "pins-changed", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			db := queueLockPostgres(t)
			f := admissionFixtureOn(t, db, "")
			_, q := reverseFixture(t, f)
			_, e := f.store.PrepareRetiredTargetResource(t.Context(), q)
			require.NoError(t, e)
			probe := reverseSyntheticProbe(t, f, q.Command)
			deadline := time.Now().Add(1500 * time.Millisecond)
			if mode == "deployment-expired" {
				f.proof.ValidUntilMs = deadline.UnixMilli()
				f.refreshSyntheticProof(t)
			}
			if mode == "freshness-expired" {
				f.proof.RevocationFreshUntilMs = deadline.UnixMilli()
				f.refreshSyntheticProof(t)
			}
			if mode == "probe-expired" {
				probe.ValidUntilMs = deadline.UnixMilli()
				hash, e := probe.Fingerprint()
				require.NoError(t, e)
				require.NoError(t, db.Model(&model.ChannelMigrationProbeRecord{}).Where("evidence_hash=?", probe.EvidenceHash).UpdateColumn("fact_hash", hash).Error)
			}
			_, e = f.store.PrepareReverseMigration(t.Context(), q.Command)
			require.NoError(t, e)
			var before model.ChannelAdmissionControl
			require.NoError(t, db.First(&before, 1).Error)
			var raw string
			require.NoError(t, db.Table("model_configs").Select("config").Scan(&raw).Error)
			pool, e := db.DB()
			require.NoError(t, e)
			holder, e := pool.BeginTx(t.Context(), nil)
			require.NoError(t, e)
			defer holder.Rollback()
			// SHARE permits the snapshot's SELECT FOR UPDATE, but blocks the final raw
			// pin UPDATE after seal. Thus this exercises final commit proof revalidation.
			_, e = holder.Exec("LOCK TABLE model_configs IN SHARE MODE")
			require.NoError(t, e)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, e := f.store.CutoverReversePrepared(ctx, q.Command); done <- e }()
			var app string
			require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&app).Error)
			require.Eventually(t, func() bool {
				var n int64
				return db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock' AND query ILIKE '%UPDATE%model_configs%'", app).Scan(&n).Error == nil && n > 0
			}, 3*time.Second, 5*time.Millisecond)
			var observed model.ChannelAdmissionControl
			require.NoError(t, db.First(&observed, 1).Error)
			require.Equal(t, before, observed)
			var p model.ChannelMigrationPrepared
			require.NoError(t, db.First(&p, "operation_id=?", q.Command.Seal.OperationID).Error)
			require.Equal(t, "prepared", p.State)
			if mode == "deployment-expired" || mode == "freshness-expired" || mode == "probe-expired" {
				if d := time.Until(deadline.Add(30 * time.Millisecond)); d > 0 {
					time.Sleep(d)
				}
			}
			if mode == "pins-changed" {
				_, e = holder.Exec("UPDATE model_configs SET config=config||' '")
				require.NoError(t, e)
				raw += " "
			}
			if mode == "cancelled" {
				cancel()
				select {
				case e = <-done:
					require.Error(t, e)
				case <-time.After(time.Second):
					t.Fatal("cancel did not exit final commit wait")
				}
				require.NoError(t, holder.Rollback())
			} else {
				require.NoError(t, holder.Commit())
				e = <-done
			}
			require.Equal(t, model.ChannelStatusEnabled, migrationStatus(t, db, q.Command.Seal.From.ChannelID))
			require.Equal(t, model.ChannelStatusEnabled, migrationStatus(t, db, q.Command.Seal.To.ChannelID))
			if mode == "live" {
				require.NoError(t, e)
				read, e := f.store.ReadPreparedMigration(t.Context(), q.Command.Seal.OperationID)
				require.NoError(t, e)
				require.Equal(t, before.Revision+2, read.CommitRevision)
				return
			}
			require.Error(t, e)
			require.NoError(t, db.First(&observed, 1).Error)
			require.Equal(t, before, observed)
			require.NoError(t, db.First(&p, "operation_id=?", q.Command.Seal.OperationID).Error)
			require.Equal(t, "prepared", p.State)
			require.Zero(t, p.SealRevision)
			require.Zero(t, p.CommitRevision)
			var after string
			require.NoError(t, db.Table("model_configs").Select("config").Scan(&after).Error)
			require.Equal(t, raw, after)
			var n int64
			require.NoError(t, db.Model(&model.ChannelAdmissionBarrier{}).Where("operation_id=?", q.Command.Seal.OperationID).Count(&n).Error)
			require.Zero(t, n)
		})
	}
}

func TestPostgresReverseResourceContenders(t *testing.T) {
	db := queueLockPostgres(t)
	f := admissionFixtureOn(t, db, "")
	_, q := reverseFixture(t, f)
	other := q
	other.Command.Seal.OperationID = "reverse-contender"
	done := make(chan error, 2)
	go func() { _, e := f.store.PrepareRetiredTargetResource(t.Context(), q); done <- e }()
	go func() { _, e := f.store.PrepareRetiredTargetResource(t.Context(), other); done <- e }()
	a, b := <-done, <-done
	require.NotEqual(t, a == nil, b == nil)
	var n int64
	require.NoError(t, db.Model(&model.ChannelMigrationRetiredTargetResource{}).Count(&n).Error)
	require.EqualValues(t, 1, n)
}
