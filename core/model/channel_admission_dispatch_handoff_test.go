package model_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func installDispatchHandoff(t *testing.T, db *gorm.DB) {
	t.Helper()
	installNativeExecution(t, db)
	raw, err := os.ReadFile("../migrations/channel-dispatch-handoff-v1/" + db.Name() + ".sql")
	require.NoError(t, err)

	var lines []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}

	parts := strings.Split(strings.TrimSpace(strings.Join(lines, "\n")), ";")
	require.Equal(t, "BEGIN", strings.TrimSpace(parts[0]))
	require.Equal(t, "COMMIT", strings.TrimSpace(parts[len(parts)-2]))

	for range 2 {
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			for _, part := range parts[1 : len(parts)-2] {
				if err := tx.Exec(strings.TrimSpace(part)).Error; err != nil {
					return err
				}
			}

			return nil
		}))
	}
}

func dispatchHandoffTask(t *testing.T, f *admissionFixture, deadline time.Time) *model.NativeTask {
	t.Helper()
	task := executionNativeTask(t, f)
	require.NoError(
		t,
		f.store.DB.Model(task).
			Updates(map[string]any{"status": model.NativeStatusDispatching, "dispatch_owner": "unique-dispatch-claim", "dispatch_until": deadline.UTC()}).
			Error,
	)
	task, err := model.GetNativeTask(f.store.DB, task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)

	return task
}

func handoffRows(t *testing.T, db *gorm.DB) []model.ChannelAdmissionDispatchHandoff {
	t.Helper()

	var rows []model.ChannelAdmissionDispatchHandoff
	require.NoError(t, db.Find(&rows).Error)

	return rows
}

func TestAdmissionDispatchHandoffSingleChildAndAtomicExit(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installDispatchHandoff(t, db)
	task := dispatchHandoffTask(t, f, time.Now().Add(time.Minute))
	h, err := model.PrepareNativeDispatchHandoff(t.Context(), db, task)
	require.NoError(t, err)
	require.NotNil(t, h)
	//nolint:staticcheck // Regression requires private authorization handles to serialize as an empty object.
	raw, err := json.Marshal(h)
	require.NoError(t, err)
	require.Equal(t, "{}", string(raw))
	require.Equal(t, "offered", handoffRows(t, db)[0].State)
	require.Equal(t, "active", nativeExecutionRows(t, db)[0].State)
	require.Error(t, h.RecordExit(t.Context(), false))
	duplicate, err := model.PrepareNativeDispatchHandoff(t.Context(), db, task)
	require.Error(t, err)
	require.Nil(t, duplicate)
	require.Len(t, nativeExecutionRows(t, db), 1)
	accepted, err := h.Accept(t.Context())
	require.NoError(t, err)
	require.True(t, accepted)
	accepted, err = h.Accept(t.Context())
	require.NoError(t, err)
	require.False(t, accepted)
	require.NoError(t, h.RecordExit(t.Context(), false))
	before := handoffRows(t, db)[0]
	require.NoError(t, h.RecordExit(t.Context(), false))
	require.Equal(t, before, handoffRows(t, db)[0])
	require.Error(t, h.RecordExit(t.Context(), true))
	require.Equal(t, "exited", before.State)
	execution := nativeExecutionRows(t, db)[0]
	require.Equal(t, "returned", execution.State)
	require.Equal(t, before.ExitedAtMs, execution.ReturnedAtMs)
	require.Equal(t, "unproven", execution.ContinuationState)
	require.Equal(t, "open", runtimeUses(t, db)[0].State)
}

func TestAdmissionDispatchHandoffPartialStorageAndAtomicOffer(t *testing.T) {
	for _, mode := range []string{"absent", "missing-table", "missing-protocol", "wrong-version", "missing-column", "offer-write-fails", "valid"} {
		t.Run(mode, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installNativeExecution(t, db)

			task := dispatchHandoffTask(t, f, time.Now().Add(time.Minute))
			if mode != "absent" {
				installDispatchHandoff(t, db)
			}

			switch mode {
			case "missing-table":
				require.NoError(
					t,
					db.Migrator().DropTable(&model.ChannelAdmissionDispatchHandoff{}),
				)
			case "missing-protocol":
				require.NoError(
					t,
					db.Where("id = ?", 3).Delete(&model.ChannelAdmissionExecutionProtocol{}).Error,
				)
			case "wrong-version":
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionExecutionProtocol{}).
						Where("id = ?", 3).
						Update("version", 99).
						Error,
				)
			case "missing-column":
				require.NoError(
					t,
					db.Exec(
						"ALTER TABLE channel_admission_dispatch_handoffs DROP COLUMN accepted_at_ms",
					).Error,
				)
			case "offer-write-fails":
				require.NoError(
					t,
					db.Callback().
						Create().
						After("gorm:create").
						Register("test:offer-failure", func(tx *gorm.DB) {
							if tx.Statement.Table == "channel_admission_dispatch_handoffs" {
								_ = tx.AddError(errors.New("synthetic write failure"))
							}
						}),
				)
				defer func() {
					if err := db.
						Callback().Create().
						Remove("test:offer-failure"); err !=
						nil {
						t.Error(err)
					}
				}()
			}

			h, err := model.PrepareNativeDispatchHandoff(t.Context(), db, task)
			switch mode {
			case "absent":
				require.NoError(t, err)
				require.Nil(t, h)
			case "valid":
				require.NoError(t, err)
				require.NotNil(t, h)
			default:
				require.Error(t, err)
				require.Nil(t, h)
			}

			if mode != "valid" {
				require.Empty(t, nativeExecutionRows(t, db))
			}

			require.Equal(t, "open", runtimeUses(t, db)[0].State)
		})
	}
}

func TestAdmissionDispatchHandoffRejectsLateAndLostCapabilities(t *testing.T) {
	for _, phase := range []string{"accept", "exit"} {
		for _, change := range []string{"owner", "expired", "lease-changed", "storage", "use", "channel", "capability", "handoff-hash", "exit-write-fails", "epoch-switch", "proof-unavailable"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				f := runtimeAdmissionFixture(t)
				db := f.store.DB
				installDispatchHandoff(t, db)
				task := dispatchHandoffTask(t, f, time.Now().Add(time.Minute))
				h, err := model.PrepareNativeDispatchHandoff(t.Context(), db, task)
				require.NoError(t, err)

				if phase == "exit" {
					accepted, err := h.Accept(t.Context())
					require.NoError(t, err)
					require.True(t, accepted)
				}

				switch change {
				case "owner":
					require.NoError(t, db.Model(task).Update("dispatch_owner", "new-claim").Error)
				case "expired":
					require.NoError(
						t,
						db.Model(task).
							Update("dispatch_until", time.Now().Add(-time.Second).UTC()).
							Error,
					)
				case "lease-changed":
					require.NoError(
						t,
						db.Model(task).
							Update("dispatch_until", time.Now().Add(2*time.Minute).UTC()).
							Error,
					)
				case "storage":
					require.NoError(
						t,
						db.Model(&model.ChannelAdmissionControl{}).
							Where("id = 1").
							Update("storage_generation", "restored").
							Error,
					)
				case "use":
					require.NoError(
						t,
						db.Model(&model.ChannelAdmissionUse{}).
							Where("id = ?", nativeExecutionRows(t, db)[0].UseID).
							Update("admitted_at_ms", 1).
							Error,
					)
				case "channel":
					require.NoError(
						t,
						db.Model(&model.Channel{}).
							Where("id = 1").
							Update("base_url", "https://other.invalid").
							Error,
					)
				case "capability":
					h = &model.NativeDispatchHandoffHandle{}
				case "handoff-hash":
					require.NoError(
						t,
						db.Model(&model.ChannelAdmissionDispatchHandoff{}).
							Where("id = ?", handoffRows(t, db)[0].ID).
							Update("authorization_hash", strings.Repeat("a", 64)).
							Error,
					)
				case "exit-write-fails":
					require.NoError(
						t,
						db.Callback().
							Update().
							After("gorm:update").
							Register("test:handoff-update", func(tx *gorm.DB) {
								if tx.Statement.Table == "channel_admission_dispatch_handoffs" {
									_ = tx.AddError(errors.New("synthetic update failed"))
								}
							}),
					)
					defer func() {
						if err := db.
							Callback().Update().
							Remove("test:handoff-update"); err !=
							nil {
							t.Error(err)
						}
					}()

				case "epoch-switch":
					restore := advanceNativeExecutionEpoch(t, f, 2, false)
					defer restore()
				case "proof-unavailable":
					restore := model.ConfigureChannelAdmissionRuntime(f.store.Executor, nil)
					defer restore()
				}

				if phase == "accept" {
					var accepted bool

					accepted, err = h.Accept(t.Context())
					if err != nil {
						require.False(t, accepted)
					}
				} else {
					err = h.RecordExit(t.Context(), false)
				}

				if change == "epoch-switch" || change == "proof-unavailable" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)

					want := "offered"
					if phase == "exit" {
						want = "accepted"
					}

					require.Equal(t, want, handoffRows(t, db)[0].State)
					require.Equal(t, "active", nativeExecutionRows(t, db)[0].State)
				}

				require.Equal(t, "open", runtimeUses(t, db)[0].State)
			})
		}
	}
}

func TestPostgresDispatchHandoffRechecksAfterLock(t *testing.T) {
	for _, phase := range []string{"accept", "exit"} {
		for _, change := range []string{"live", "expired", "owner"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				db := queueLockPostgres(t)
				f := runtimeAdmissionOnFixture(t, admissionFixtureOn(t, db, ""))
				installDispatchHandoff(t, db)

				deadline := time.Now().Add(2 * time.Second)
				task := dispatchHandoffTask(t, f, deadline)
				h, err := model.PrepareNativeDispatchHandoff(t.Context(), db, task)
				require.NoError(t, err)

				if phase == "exit" {
					accepted, err := h.Accept(t.Context())
					require.NoError(t, err)
					require.True(t, accepted)
				}

				pool, err := db.DB()
				require.NoError(t, err)

				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()

				holder, err := pool.BeginTx(ctx, nil)
				require.NoError(t, err)

				defer func() {
					if err := holder.
						Rollback(); err !=
						nil && !errors.
						Is(err,
							sql.ErrTxDone,
						) {
						t.Error(err)
					}
				}()

				var id string
				require.NoError(
					t,
					holder.QueryRowContext(ctx, "SELECT id FROM native_tasks WHERE id=$1 FOR UPDATE", task.ID).
						Scan(&id),
				)

				done := make(chan error, 1)
				go func() {
					if phase == "accept" {
						_, err := h.Accept(ctx)
						done <- err
					} else {
						done <- h.RecordExit(ctx, false)
					}
				}()

				var application string
				require.NoError(
					t,
					db.Raw("SELECT current_setting('application_name')").Scan(&application).Error,
				)
				require.Eventually(t, func() bool {
					var count int64

					err := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", application).
						Scan(&count).
						Error

					return err == nil && count > 0
				}, time.Second, 5*time.Millisecond)

				if change == "expired" {
					if delay := time.Until(deadline.Add(30 * time.Millisecond)); delay > 0 {
						time.Sleep(delay)
					}
				}

				if change == "owner" {
					_, err = holder.ExecContext(
						ctx,
						"UPDATE native_tasks SET dispatch_owner=$1 WHERE id=$2",
						"new-claim",
						task.ID,
					)
					require.NoError(t, err)
				}

				require.NoError(t, holder.Commit())

				err = <-done
				if change == "live" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)

					want := "offered"
					if phase == "exit" {
						want = "accepted"
					}

					require.Equal(t, want, handoffRows(t, db)[0].State)
					require.Equal(t, "active", nativeExecutionRows(t, db)[0].State)
				}
			})
		}
	}
}

func TestAdmissionDispatchHandoffConcurrentAcceptAndReclaim(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installDispatchHandoff(t, db)
	task := dispatchHandoffTask(t, f, time.Now().Add(time.Minute))
	h, err := model.PrepareNativeDispatchHandoff(t.Context(), db, task)
	require.NoError(t, err)

	type result struct {
		accepted bool
		err      error
	}

	results := make(chan result, 2)
	for range 2 {
		go func() { accepted, err := h.Accept(context.Background()); results <- result{accepted, err} }()
	}

	acceptedCount := 0
	for range 2 {
		r := <-results
		require.NoError(t, r.err)

		if r.accepted {
			acceptedCount++
		}
	}

	require.Equal(t, 1, acceptedCount)
	// A different unique dispatch claim can be prepared, but the old child
	// capability cannot borrow it or finish its new execution.
	require.NoError(
		t,
		db.Model(task).
			Updates(map[string]any{"dispatch_owner": "new-unique-claim", "dispatch_until": time.Now().Add(2 * time.Minute).UTC()}).
			Error,
	)
	nextTask, err := model.GetNativeTask(db, task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)
	next, err := model.PrepareNativeDispatchHandoff(t.Context(), db, nextTask)
	require.NoError(t, err)
	require.NotNil(t, next)
	oldAccepted, err := h.Accept(t.Context())
	require.Error(t, err)
	require.False(t, oldAccepted)
	require.Error(t, h.RecordExit(t.Context(), false))
	accepted, err := next.Accept(t.Context())
	require.NoError(t, err)
	require.True(t, accepted)
	require.NoError(t, next.RecordExit(t.Context(), false))
	require.Len(t, handoffRows(t, db), 2)
	require.Equal(t, "open", runtimeUses(t, db)[0].State)
}

func TestAdmissionDispatchHandoffExpiryDuringWriteRollsBack(t *testing.T) {
	for _, phase := range []string{"offer", "accept", "exit"} {
		t.Run(phase, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installDispatchHandoff(t, db)

			deadline := time.Now().Add(1500 * time.Millisecond)
			task := dispatchHandoffTask(t, f, deadline)

			var (
				h   *model.NativeDispatchHandoffHandle
				err error
			)
			if phase != "offer" {
				h, err = model.PrepareNativeDispatchHandoff(t.Context(), db, task)
				require.NoError(t, err)
			}

			if phase == "exit" {
				accepted, err := h.Accept(t.Context())
				require.NoError(t, err)
				require.True(t, accepted)
			}

			pause := func(tx *gorm.DB) {
				if tx.Statement.Table == "channel_admission_dispatch_handoffs" {
					if delay := time.Until(deadline.Add(30 * time.Millisecond)); delay > 0 {
						time.Sleep(delay)
					}
				}
			}
			if phase == "offer" {
				require.NoError(
					t,
					db.Callback().
						Create().
						After("gorm:create").
						Register("test:handoff-expire", pause),
				)
				defer func() {
					if err := db.
						Callback().Create().
						Remove("test:handoff-expire"); err !=
						nil {
						t.Error(err)
					}
				}()

				_, err = model.PrepareNativeDispatchHandoff(t.Context(), db, task)
			} else {
				require.NoError(
					t,
					db.Callback().
						Update().
						After("gorm:update").
						Register("test:handoff-expire", pause),
				)
				defer func() {
					if err := db.
						Callback().Update().
						Remove("test:handoff-expire"); err !=
						nil {
						t.Error(err)
					}
				}()

				if phase == "accept" {
					accepted, e := h.Accept(t.Context())
					require.False(t, accepted)

					err = e
				} else {
					err = h.RecordExit(t.Context(), false)
				}
			}

			require.Error(t, err)

			if phase == "offer" {
				require.Empty(t, handoffRows(t, db))
				require.Empty(t, nativeExecutionRows(t, db))
			} else {
				want := "offered"
				if phase == "exit" {
					want = "accepted"
				}

				require.Equal(t, want, handoffRows(t, db)[0].State)
				require.Equal(t, "active", nativeExecutionRows(t, db)[0].State)
			}
		})
	}
}

func TestAdmissionDispatchHandoffOrphanProtocolCannotLookAbsent(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installExecutionJournal(t, db)
	require.NoError(t, db.Create(&model.ChannelAdmissionExecutionProtocol{ID: 3, Version: 1}).Error)
	task := dispatchHandoffTask(t, f, time.Now().Add(time.Minute))
	h, err := model.PrepareNativeDispatchHandoff(t.Context(), db, task)
	require.Error(t, err)
	require.Nil(t, h)
	require.Equal(t, "open", runtimeUses(t, db)[0].State)
}

func TestAdmissionDispatchHandoffPrepareKeepsOriginalClaimDeadline(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installDispatchHandoff(t, db)
	task := dispatchHandoffTask(t, f, time.Now().Add(time.Minute))
	require.NoError(
		t,
		db.Model(&model.NativeTask{}).
			Where("id = ?", task.ID).
			Update("dispatch_until", time.Now().Add(2*time.Minute).UTC()).
			Error,
	)
	h, err := model.PrepareNativeDispatchHandoff(t.Context(), db, task)
	require.Error(t, err)
	require.Nil(t, h)
	require.Empty(t, nativeExecutionRows(t, db))
	require.Empty(t, handoffRows(t, db))
}
