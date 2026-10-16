package task

import (
	"context"
	"fmt"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"
)

func renewalTaskPostgres(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("B2_G3_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires a disposable local PostgreSQL via B2_G3_TEST_POSTGRES_DSN")
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"127.0.0.1", "localhost", "::1"}, u.Hostname())
	require.Equal(t, "/g3_queue_test", u.Path)
	admin, err := model.OpenPostgreSQL(dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("renewal_stop_%d", time.Now().UnixNano())
	require.NoError(t, admin.Exec("CREATE SCHEMA "+schema).Error)
	query := u.Query()
	query.Set("search_path", schema)
	query.Set("application_name", schema)
	u.RawQuery = query.Encode()
	db, err := model.OpenPostgreSQL(u.String())
	require.NoError(t, err)
	t.Cleanup(func() {
		pool, e := db.DB()
		require.NoError(t, e)
		require.NoError(t, pool.Close())
		require.NoError(t, admin.Exec("DROP SCHEMA "+schema+" CASCADE").Error)
		pool, e = admin.DB()
		require.NoError(t, e)
		require.NoError(t, pool.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.AsyncUsageInfo{}))
	return db
}

func TestPostgresAsyncUsageRenewalStopJoinsSQLWait(t *testing.T) {
	for _, mode := range []string{"stop", "parent-cancel"} {
		t.Run(mode, func(t *testing.T) {
			db := renewalTaskPostgres(t)
			old := model.LogDB
			model.LogDB = db
			t.Cleanup(func() { model.LogDB = old })
			info := &model.AsyncUsageInfo{Status: model.AsyncUsageStatusPending, ProcessingToken: "synthetic-token", NextPollAt: time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)}
			require.NoError(t, db.Create(info).Error)
			pool, err := db.DB()
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			holder, err := pool.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer holder.Rollback()
			var id int
			require.NoError(t, holder.QueryRowContext(ctx, "SELECT id FROM async_usage_infos WHERE id=$1 FOR UPDATE", info.ID).Scan(&id))
			parent, cancelParent := context.WithCancel(ctx)
			defer cancelParent()
			_, stop := startAsyncUsageClaimRenewalAtInterval(parent, info, time.Millisecond)
			var app string
			require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&app).Error)
			require.Eventually(t, func() bool {
				var n int64
				e := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).Scan(&n).Error
				return e == nil && n > 0
			}, time.Second, 5*time.Millisecond)
			if mode == "parent-cancel" {
				cancelParent()
			}
			var wg sync.WaitGroup
			for range 4 {
				wg.Add(1)
				go func() { defer wg.Done(); stop() }()
			}
			joined := make(chan struct{})
			go func() { wg.Wait(); close(joined) }()
			select {
			case <-joined:
			case <-time.After(time.Second):
				t.Fatal("stop did not cancel and join SQL wait")
			}
			// Holder still owns the row: stop did not merely wait for it to become free.
			require.NoError(t, holder.Commit())
			stop()
			var row model.AsyncUsageInfo
			require.NoError(t, db.First(&row, info.ID).Error)
			require.True(t, row.NextPollAt.Equal(info.NextPollAt))
			require.Equal(t, info.ProcessingToken, row.ProcessingToken)
		})
	}
}
