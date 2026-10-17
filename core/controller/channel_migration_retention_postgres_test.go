package controller

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestPostgresMigrationRetentionConsistentSnapshot(t *testing.T) {
	retentionConsistentSnapshot(t, newProbeHTTPFixtureDB(t, cutoverHTTPPostgres(t)))
}
func TestPostgresMigrationRetentionNoDML(t *testing.T) {
	f := newProbeHTTPFixtureDB(t, cutoverHTTPPostgres(t))
	retentionCommitted(t, f)
	require.NoError(t, f.db.Exec(`CREATE FUNCTION retention_no_dml() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'retention must not write'; END $$`).Error)
	var tables []string
	require.NoError(t, f.db.Raw("SELECT tablename FROM pg_tables WHERE schemaname=current_schema()").Scan(&tables).Error)
	for i, table := range tables {
		require.NoError(t, f.db.Exec(fmt.Sprintf("CREATE TRIGGER retention_no_dml_%d BEFORE INSERT OR UPDATE OR DELETE ON %s FOR EACH STATEMENT EXECUTE FUNCTION retention_no_dml()", i, table)).Error)
	}
	r := retentionRead(t, f)
	require.Equal(t, "ready", r.State)
	require.Equal(t, "unproven", r.Retention)
	require.Error(t, f.db.Exec("UPDATE channel_admission_controls SET revision=revision").Error)
	require.Equal(t, r.Receipt, retentionRead(t, f).Receipt)
}
func TestPostgresMigrationRetentionCurrentHeadSerialize(t *testing.T) {
	f := newProbeHTTPFixtureDB(t, cutoverHTTPPostgres(t))
	retentionCommitted(t, f)
	reached, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	require.NoError(t, f.db.Callback().Query().After("gorm:query").Register("retention_head_pause", func(tx *gorm.DB) {
		if tx.Statement.Table == "channel_admission_uses" && once.CompareAndSwap(false, true) {
			close(reached)
			<-release
		}
	}))
	defer f.db.Callback().Query().Remove("retention_head_pause")
	read := make(chan model.MigrationRetentionResult, 1)
	go func() { read <- retentionRead(t, f) }()
	select {
	case <-reached:
	case <-time.After(3 * time.Second):
		t.Fatal("missing read snapshot")
	}
	pool, e := f.db.DB()
	require.NoError(t, e)
	writer, e := pool.BeginTx(t.Context(), nil)
	require.NoError(t, e)
	defer writer.Rollback()
	done := make(chan error, 1)
	go func() {
		_, e := writer.Exec("UPDATE channel_admission_controls SET revision=revision+1 WHERE id=1")
		if e == nil {
			_, e = writer.Exec("UPDATE channel_admission_families SET operation_id='superseded'")
		}
		if e == nil {
			e = writer.Commit()
		}
		done <- e
	}()
	var app string
	require.NoError(t, f.db.Raw("SELECT current_setting('application_name')").Scan(&app).Error)
	require.Eventually(t, func() bool {
		var n int64
		e := f.db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock' AND query ILIKE '%UPDATE channel_admission_controls%'", app).Scan(&n).Error
		return e == nil && n > 0
	}, 2*time.Second, 10*time.Millisecond)
	close(release)
	r := <-read
	require.Equal(t, "ready", r.State)
	require.Equal(t, "committed_current", r.Cutover)
	require.NoError(t, <-done)
	r = retentionRead(t, f)
	require.Equal(t, "conflict", r.State)
	require.Nil(t, r.Uses)
}
