package model_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestPostgresMigrationShadowLockWaitSource(t *testing.T) {
	for _, change := range []string{"control-rollback", "control-channel", "control-config", "control-add", "channel-edit", "channel-delete", "config-edit", "cancel"} {
		t.Run(change, func(t *testing.T) {
			db := queueLockPostgres(t)
			f := admissionFixtureOn(t, db, "shadow-lock")
			req := shadowFixture(t, f)
			pool, e := db.DB()
			require.NoError(t, e)
			holder, e := pool.BeginTx(t.Context(), nil)
			require.NoError(t, e)

			defer func() {
				if err := holder.
					Rollback(); err !=
					nil &&
					!errors.
						Is(err,
							sql.ErrTxDone,
						) {
					t.Error(err)
				}
			}()

			switch change {
			case "channel-edit", "channel-delete":
				_, e = holder.ExecContext(t.Context(),

					"UPDATE channels SET name=name WHERE id=1")
			case "config-edit":
				_, e = holder.ExecContext(t.Context(),

					"UPDATE model_configs SET rpm=rpm WHERE model='example'")
			default:
				_, e = holder.ExecContext(t.Context(),

					"UPDATE channel_admission_controls SET revision=revision WHERE id=1",
				)
			}

			require.NoError(t, e)

			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()

			done := make(chan model.MigrationShadowResult, 1)
			go func() { done <- f.store.CreateMigrationShadow(ctx, req, shadowTestKey, nil) }()

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
			}, 2*time.Second, 5*time.Millisecond)

			switch change {
			case "control-rollback":
				require.NoError(t, holder.Rollback())
			case "control-channel", "channel-edit":
				_, e = holder.ExecContext(t.Context(),

					"UPDATE channels SET priority=99 WHERE id=1")
				require.NoError(t, e)
				require.NoError(t, holder.Commit())
			case "control-config", "config-edit":
				_, e = holder.ExecContext(t.Context(),

					"UPDATE model_configs SET rpm=99")
				require.NoError(t, e)
				require.NoError(t, holder.Commit())
			case "control-add":
				_, e = holder.ExecContext(t.Context(),

					"INSERT INTO channels(id,name,status) VALUES(777,'foreign',2)")
				require.NoError(t, e)
				require.NoError(t, holder.Commit())
			case "channel-delete":
				_, e = holder.ExecContext(t.Context(),

					"DELETE FROM channels WHERE id=1")
				require.NoError(t, e)
				require.NoError(t, holder.Commit())
			case "cancel":
				cancel()
			}

			var got model.MigrationShadowResult
			select {
			case got = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("command failed to finish")
			}

			if change == "control-rollback" {
				require.Equal(t, "ready", got.State)
				require.Equal(t, int64(1), countShadows(t, f))
			} else {
				require.NotEqual(t, "ready", got.State)

				if change == "cancel" {
					require.NoError(t, holder.Rollback())
				}

				require.Zero(t, countShadows(t, f))
			}
		})
	}
}

func TestPostgresMigrationShadowConcurrentCreate(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(strconv.FormatBool(same), func(t *testing.T) {
			db := queueLockPostgres(t)
			f := admissionFixtureOn(t, db, "shadow-concurrent")
			req := shadowFixture(t, f)

			other := req
			if !same {
				other.OperationID = "second-operation"
			}

			start := make(chan struct{})
			results := make(chan model.MigrationShadowResult, 2)

			var wg sync.WaitGroup
			for _, r := range []model.MigrationShadowRequest{req, other} {
				wg.Add(1)
				go func(input model.MigrationShadowRequest) {
					defer wg.Done()

					<-start

					results <- f.store.CreateMigrationShadow(context.Background(), input, shadowTestKey, nil)
				}(r)
			}

			close(start)
			wg.Wait()
			close(results)

			ready := 0

			id := 0
			for got := range results {
				if got.State == "ready" {
					ready++

					if id == 0 {
						id = got.Receipt.Shadow.ChannelID
					}

					require.Equal(t, id, got.Receipt.Shadow.ChannelID)
				}
			}

			if same {
				require.Equal(t, 2, ready)
			} else {
				require.Equal(t, 1, ready)
			}

			require.Equal(t, int64(1), countShadows(t, f))
		})
	}
}

func TestPostgresMigrationShadowRollbackAndReadOnly(t *testing.T) {
	db := queueLockPostgres(t)
	f := admissionFixtureOn(t, db, "shadow-rollback")
	req := shadowFixture(t, f)
	require.NoError(
		t,
		db.Exec(
			`CREATE FUNCTION reject_shadow_resource() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic rollback'; END $$`,
		).Error,
	)
	require.NoError(
		t,
		db.Exec(
			`CREATE TRIGGER reject_shadow_resource BEFORE INSERT ON channel_migration_shadow_resources FOR EACH STATEMENT EXECUTE FUNCTION reject_shadow_resource()`,
		).Error,
	)
	require.NotEqual(
		t,
		"ready",
		f.store.CreateMigrationShadow(context.Background(), req, shadowTestKey, nil).State,
	)
	require.Zero(t, countShadows(t, f))

	var n int64
	require.NoError(t, db.Model(&model.Channel{}).Count(&n).Error)
	require.Equal(t, int64(5), n)
	require.NoError(
		t,
		db.Exec("DROP TRIGGER reject_shadow_resource ON channel_migration_shadow_resources").Error,
	)

	got := f.store.CreateMigrationShadow(context.Background(), req, shadowTestKey, nil)
	require.Equal(t, "ready", got.State)

	for _, table := range []string{"channels", "model_configs", "channel_admission_controls", "channel_migration_shadow_resources"} {
		require.NoError(
			t,
			db.Exec(
				fmt.Sprintf(
					"CREATE TRIGGER shadow_read_only BEFORE INSERT OR UPDATE OR DELETE ON %s FOR EACH STATEMENT EXECUTE FUNCTION reject_shadow_resource()",
					table,
				),
			).Error,
		)
	}

	require.Equal(t, got, f.store.ReadMigrationShadow(context.Background(), req.OperationID, nil))
	require.Equal(
		t,
		"absent",
		f.store.ReadMigrationShadow(context.Background(), "missing", nil).State,
	)
}

func TestPostgresMigrationShadowOrdinaryInsertSharesControl(t *testing.T) {
	db := queueLockPostgres(t)
	f := admissionFixtureOn(t, db, "shadow-ordinary")
	req := shadowFixture(t, f)
	oldDB, oldLog := model.DB, model.LogDB
	model.DB, model.LogDB = db, db
	t.Cleanup(func() { model.DB, model.LogDB = oldDB, oldLog })

	pool, e := db.DB()
	require.NoError(t, e)
	holder, e := pool.BeginTx(t.Context(), nil)
	require.NoError(t, e)

	defer func() {
		if err := holder.
			Rollback(); err !=
			nil &&
			!errors.
				Is(err,
					sql.ErrTxDone,
				) {
			t.Error(err)
		}
	}()

	_, e = holder.ExecContext(t.Context(),

		"UPDATE channel_admission_controls SET revision=revision WHERE id=1")
	require.NoError(t, e)

	done := make(chan error, 1)
	go func() {
		done <- model.BatchInsertChannels([]*model.Channel{{Name: "ordinary-new", Key: "synthetic-ordinary", Type: 59, Status: model.ChannelStatusDisabled}})
	}()

	var app string
	require.NoError(t, db.Raw("SELECT current_setting('application_name')").Scan(&app).Error)
	require.Eventually(t, func() bool {
		var n int64

		return db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).
			Scan(&n).
			Error == nil &&
			n > 0
	}, 2*time.Second, 5*time.Millisecond)

	select {
	case earlyErr := <-done:
		t.Logf("unexpected early result: %v", earlyErr)
		t.Fatal("ordinary create bypassed control")
	default:
	}

	require.NoError(t, holder.Commit())

	select {
	case e = <-done:
		require.NoError(t, e)
	case <-time.After(3 * time.Second):
		t.Fatal("ordinary create did not exit")
	}

	require.Equal(
		t,
		"conflict",
		f.store.CreateMigrationShadow(context.Background(), req, shadowTestKey, nil).State,
	)
	require.Zero(t, countShadows(t, f))
}

func TestPostgresMigrationShadowSQLDraft(t *testing.T) {
	db := queueLockPostgres(t)
	f := admissionFixtureOn(t, db, "shadow-draft")
	req := shadowFixture(t, f)
	require.NoError(
		t,
		db.Migrator().
			DropTable(&model.ChannelMigrationShadowResource{}, &model.ChannelMigrationShadowProtocol{}),
	)

	raw, e := os.ReadFile("../../docs/sql/b2-shadow-resources-postgres-draft.sql")
	require.NoError(t, e)
	pool, e := db.DB()
	require.NoError(t, e)
	conn, e := pool.Conn(context.Background())
	require.NoError(t, e)

	defer conn.Close()
	defer func() {
		if _, err := conn.ExecContext(context.
			Background(), "ROLLBACK",
		); err !=
			nil {
			t.Error(
				err,
			)
		}
	}()

	for statement := range strings.SplitSeq(string(raw), ";") {
		if strings.TrimSpace(statement) != "" {
			_, e = conn.ExecContext(context.Background(), statement)
			require.NoError(t, e)
		}
	}

	got := f.store.CreateMigrationShadow(context.Background(), req, shadowTestKey, nil)
	require.Equal(t, "ready", got.State)
}
