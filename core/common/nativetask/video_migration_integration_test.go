package nativetask

import (
	"context"
	"encoding/json"
	"fmt"
	"gorm.io/gorm"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func setupVideoMigration(t *testing.T) (*Engine, *walletStub) {
	t.Helper()
	e, p, w, _ := setup(t)
	if dsn := os.Getenv("B2_VIDEO_TEST_POSTGRES_DSN"); dsn != "" {
		u, err := url.Parse(dsn)
		require.NoError(t, err)
		require.Equal(t, "127.0.0.1", u.Hostname())
		require.Equal(t, "/g3_queue_test", u.Path)
		admin, err := model.OpenPostgreSQL(dsn)
		require.NoError(t, err)
		schema := fmt.Sprintf("video_migration_%d", time.Now().UnixNano())
		require.NoError(t, admin.Exec("CREATE SCHEMA "+schema).Error)
		q := u.Query()
		q.Set("search_path", schema)
		q.Set("application_name", schema)
		u.RawQuery = q.Encode()
		db, err := model.OpenPostgreSQL(u.String())
		require.NoError(t, err)
		e.DB = db
		require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
		t.Cleanup(func() {
			pool, _ := db.DB()
			require.NoError(t, pool.Close())
			require.NoError(t, admin.Exec("DROP SCHEMA "+schema+" CASCADE").Error)
			pool, _ = admin.DB()
			require.NoError(t, pool.Close())
		})
	}
	var contract nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(p.Contract, &contract))
	contract.Artifacts = []nativeresult.ArtifactBinding{{Path: []string{"file", "url"}}}
	p.Contract, _ = json.Marshal(contract)
	p.DeliveryBase = "https://gateway.example"
	activateNativeRuntime(t, e, &p)
	installNativeInvocationJournal(t, e)
	installRecoveryWorkerHandoff(t, e)
	_, err := e.Submit(t.Context(), "video-migration", "g", 1, body, p)
	require.NoError(t, err)
	require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "video-migration").Update("next_recovery_at", 0).Error)
	return e, w
}

// Exercise the actual RecoverOnce -> Poll -> Deliver -> SyncBilling -> handoff
// chain. The existing two-minute claim is near expiry by supplying a historical
// scheduler timestamp; no production timeout or clock is replaced. Archive keeps
// its actual ten-minute context and protected eleven-minute extension.
func TestVideoMigrationActualRecoveryLongDelivery(t *testing.T) {
	e, _ := setupVideoMigration(t)
	var initialUntil int64
	var originalOwner string
	poll := recoveryPollFunc(func(ctx context.Context) (nativeresult.PollResult, error) {
		row, err := model.GetNativeTask(e.DB, "video-migration", "g", 1)
		require.NoError(t, err)
		originalOwner, initialUntil = row.RecoveryOwner, row.RecoveryUntil
		require.Len(t, originalOwner, 64)
		require.NotEqual(t, "video-worker", originalOwner)
		return nativeresult.PollResult{Status: "result_received", Output: []byte(`{"file":{"url":"https://provider.example/video.mp4"}}`)}, nil
	})
	called := false
	report, err := e.RecoverOnce(t.Context(), "video-worker", time.Now().Add(-117*time.Second), recoveryResolver(poll), func(ctx context.Context, id string, index int, source string) (ownedartifact.Receipt, error) {
		called = true
		row, err := model.GetNativeTask(e.DB, id, "g", 1)
		require.NoError(t, err)
		require.Equal(t, originalOwner, row.RecoveryOwner)
		require.Greater(t, row.RecoveryUntil, time.Now().Add(10*time.Minute).Unix())
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.Greater(t, time.Until(deadline), 9*time.Minute)
		if delay := time.Until(time.Unix(initialUntil, 0).Add(30 * time.Millisecond)); delay > 0 {
			time.Sleep(delay)
		}
		require.NoError(t, ctx.Err())
		assertRecoveryOuter(t, e, "accepted")
		return chunkedReceipt(id, mp4Bytes(), "video/mp4"), nil
	})
	require.NoError(t, err)
	require.True(t, called)
	require.Equal(t, 1, report.Advanced)
	row, err := model.GetNativeTask(e.DB, "video-migration", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "completed", row.Status)
	require.Empty(t, row.RecoveryOwner)
	require.Zero(t, row.RecoveryUntil)
	require.NotEmpty(t, row.ArtifactManifest)
	require.Contains(t, row.DeliveredOutput, "gateway.example")
	assertRecoveryOuter(t, e, "exited")
	for _, r := range nativeInvocations(t, e) {
		require.NotEqual(t, "active", r.State)
		require.Equal(t, "unproven", r.ContinuationState)
	}
}

func TestVideoMigrationActualLostOwnerAndCancellation(t *testing.T) {
	for _, mode := range []string{"owner-before-extension", "owner-in-archive", "cancel-in-cleanup", "late-wallet-receipt"} {
		t.Run(mode, func(t *testing.T) {
			e, w := setupVideoMigration(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			replace := func() {
				require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "video-migration").Updates(map[string]any{"recovery_owner": "replacement", "recovery_until": time.Now().Add(time.Minute).Unix()}).Error)
			}
			poll := recoveryPollFunc(func(ctx context.Context) (nativeresult.PollResult, error) {
				if mode == "owner-before-extension" {
					replace()
				}
				return nativeresult.PollResult{Status: "result_received", Output: []byte(`{"file":{"url":"https://provider.example/video.mp4"}}`)}, nil
			})
			if mode == "late-wallet-receipt" {
				e.Wallet = recoveryWalletFunc(func(ctx context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
					if c.Action == "get" {
						replace()
						return balance.PrepaymentReceipt{ID: c.BillingOperationID, Status: "settled"}, nil
					}
					return w.Prepayment(ctx, c)
				})
			}
			archiveCalls := 0
			_, err := e.RecoverOnce(ctx, "video-worker", time.Now(), recoveryResolver(poll), func(ctx context.Context, id string, index int, source string) (ownedartifact.Receipt, error) {
				archiveCalls++
				if mode == "owner-in-archive" {
					replace()
				}
				if mode == "cancel-in-cleanup" {
					defer cancel()
				}
				return chunkedReceipt(id, mp4Bytes(), "video/mp4"), nil
			})
			row, readErr := model.GetNativeTask(e.DB, "video-migration", "g", 1)
			require.NoError(t, readErr)
			require.Empty(t, row.ArtifactManifest)
			require.Empty(t, row.DeliveredOutput)
			require.False(t, row.BillingSettled)
			require.NotContains(t, row.BillingReceiptJSON, `"status":"settled"`)
			require.False(t, refundedWith(w, "platform_failure"))
			if mode == "cancel-in-cleanup" {
				// RecoverOnce finishes the owned handoff/exit, then its next loop
				// respects the cancelled parent instead of claiming more work.
				require.ErrorIs(t, err, context.Canceled)
				assertRecoveryOuter(t, e, "exited")
			} else {
				require.Error(t, err)
				require.Equal(t, "replacement", row.RecoveryOwner)
				assertRecoveryOuter(t, e, "accepted")
			}
			if mode == "owner-before-extension" || mode == "late-wallet-receipt" {
				require.Zero(t, archiveCalls)
			} else {
				require.Equal(t, 1, archiveCalls)
			}
		})
	}
}

// A real second PostgreSQL connection blocks the production receipt transaction.
// Its delayed response cannot borrow a replacement or revive an expired lease.
func TestVideoMigrationPostgresLateReceiptLock(t *testing.T) {
	if os.Getenv("B2_VIDEO_TEST_POSTGRES_DSN") == "" {
		t.Skip("requires isolated loopback PostgreSQL")
	}
	for _, outcome := range []string{"live", "expired", "replacement"} {
		t.Run(outcome, func(t *testing.T) {
			e, w := setupVideoMigration(t)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			held := make(chan struct{})
			release := make(chan struct{})
			var holderErr error
			lockedOnce := false
			e.Wallet = recoveryWalletFunc(func(ctx context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
				if c.Action != "get" {
					return w.Prepayment(ctx, c)
				}
				if lockedOnce {
					return balance.PrepaymentReceipt{ID: c.BillingOperationID, Status: "settled"}, nil
				}
				lockedOnce = true
				pool, err := e.DB.DB()
				if err != nil {
					return balance.PrepaymentReceipt{}, err
				}
				tx, err := pool.BeginTx(ctx, nil)
				if err != nil {
					return balance.PrepaymentReceipt{}, err
				}
				var id string
				err = tx.QueryRowContext(ctx, "SELECT id FROM native_tasks WHERE id=$1 FOR UPDATE", "video-migration").Scan(&id)
				if err != nil {
					_ = tx.Rollback()
					return balance.PrepaymentReceipt{}, err
				}
				if outcome == "expired" {
					_, err = tx.ExecContext(ctx, "UPDATE native_tasks SET recovery_until=$1 WHERE id=$2", time.Now().Add(2*time.Second).Unix(), id)
				}
				if outcome == "replacement" {
					_, err = tx.ExecContext(ctx, "UPDATE native_tasks SET recovery_owner=$1 WHERE id=$2", "replacement", id)
				}
				if err != nil {
					_ = tx.Rollback()
					return balance.PrepaymentReceipt{}, err
				}
				go func() { <-release; holderErr = tx.Commit(); close(held) }()
				return balance.PrepaymentReceipt{ID: c.BillingOperationID, Status: "settled"}, nil
			})
			done := make(chan error, 1)
			poll := recoveryPollFunc(func(context.Context) (nativeresult.PollResult, error) {
				return nativeresult.PollResult{Status: "running"}, nil
			})
			go func() {
				_, err := e.RecoverOnce(ctx, "video-worker", time.Now(), recoveryResolver(poll), nil)
				done <- err
			}()
			require.Eventually(t, func() bool {
				var n int64
				err := e.DB.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=current_setting('application_name') AND wait_event_type='Lock'").Scan(&n).Error
				return err == nil && n > 0
			}, 4*time.Second, 10*time.Millisecond)
			if outcome == "expired" {
				time.Sleep(2200 * time.Millisecond)
			}
			close(release)
			<-held
			require.NoError(t, holderErr)
			err := <-done
			row, readErr := model.GetNativeTask(e.DB, "video-migration", "g", 1)
			require.NoError(t, readErr)
			if outcome == "live" {
				require.NoError(t, err)
				require.True(t, row.BillingSettled)
				assertRecoveryOuter(t, e, "exited")
			} else {
				require.Error(t, err)
				require.False(t, row.BillingSettled)
				require.Empty(t, row.BillingReceiptJSON)
				assertRecoveryOuter(t, e, "accepted")
			}
		})
	}
}

func TestVideoMigrationPostgresExtensionLock(t *testing.T) {
	if os.Getenv("B2_VIDEO_TEST_POSTGRES_DSN") == "" {
		t.Skip("requires isolated loopback PostgreSQL")
	}
	for _, outcome := range []string{"live", "expired"} {
		t.Run(outcome, func(t *testing.T) {
			e, _ := setupVideoMigration(t)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			locked := make(chan func() error, 1)
			var once sync.Once
			require.NoError(t, e.DB.Callback().Query().Before("gorm:query").Register("test:video-extension-lock", func(query *gorm.DB) {
				if query.Statement.Table != "native_tasks" || len(query.Statement.Selects) != 1 || query.Statement.Selects[0] != "id" || !strings.Contains(fmt.Sprint(query.Statement.Clauses["WHERE"].Expression), "recovery_owner") {
					return
				}
				once.Do(func() {
					pool, err := e.DB.DB()
					if err != nil {
						query.AddError(err)
						return
					}
					holder, err := pool.BeginTx(ctx, nil)
					if err != nil {
						query.AddError(err)
						return
					}
					var id string
					err = holder.QueryRowContext(ctx, "SELECT id FROM native_tasks WHERE id=$1 FOR UPDATE", "video-migration").Scan(&id)
					if err != nil {
						holder.Rollback()
						query.AddError(err)
						return
					}
					locked <- holder.Commit
				})
			}))
			t.Cleanup(func() { e.DB.Callback().Query().Remove("test:video-extension-lock") })
			poll := recoveryPollFunc(func(context.Context) (nativeresult.PollResult, error) {
				return nativeresult.PollResult{Status: "result_received", Output: []byte(`{"file":{"url":"https://provider.example/video.mp4"}}`)}, nil
			})
			archived := false
			done := make(chan error, 1)
			go func() {
				_, err := e.RecoverOnce(ctx, "video-worker", time.Now().Add(-116*time.Second), recoveryResolver(poll), func(_ context.Context, id string, _ int, _ string) (ownedartifact.Receipt, error) {
					archived = true
					return chunkedReceipt(id, mp4Bytes(), "video/mp4"), nil
				})
				done <- err
			}()
			var commit func() error
			select {
			case commit = <-locked:
			case <-ctx.Done():
				t.Fatal("actual ExtendNativeRecovery lock not reached")
			}
			require.Eventually(t, func() bool {
				var n int64
				err := e.DB.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=current_setting('application_name') AND wait_event_type='Lock'").Scan(&n).Error
				return err == nil && n > 0
			}, 2*time.Second, 10*time.Millisecond)
			if outcome == "expired" {
				time.Sleep(4100 * time.Millisecond)
			}
			require.NoError(t, commit())
			err := <-done
			row, readErr := model.GetNativeTask(e.DB, "video-migration", "g", 1)
			require.NoError(t, readErr)
			if outcome == "live" {
				require.NoError(t, err)
				require.True(t, archived)
				require.Equal(t, "completed", row.Status)
				assertRecoveryOuter(t, e, "exited")
			} else {
				require.Error(t, err)
				require.False(t, archived)
				require.Equal(t, "result_received", row.Status)
				require.LessOrEqual(t, row.RecoveryUntil, time.Now().Unix())
				require.Empty(t, row.ArtifactManifest)
				assertRecoveryOuter(t, e, "accepted")
			}
		})
	}
}
