package model_test

import (
	"context"
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

func installRecoveryHandoff(t *testing.T, db *gorm.DB) {
	t.Helper()
	installNativeExecution(t, db)
	raw, err := os.ReadFile("../migrations/channel-recovery-handoff-v1/" + db.Name() + ".sql")
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

func recoveryHandoffTask(t *testing.T, f *admissionFixture, deadline time.Time) *model.NativeTask {
	t.Helper()
	task := executionNativeTask(t, f)
	require.NoError(
		t,
		f.store.DB.Model(task).
			Updates(map[string]any{"status": "queued", "recovery_owner": "unique-recovery-claim", "recovery_until": deadline.Unix()}).
			Error,
	)
	task, err := model.GetNativeTask(f.store.DB, task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)

	return task
}

func recoveryHandoffRows(t *testing.T, db *gorm.DB) []model.ChannelAdmissionRecoveryHandoff {
	t.Helper()

	var rows []model.ChannelAdmissionRecoveryHandoff
	require.NoError(t, db.Order("id").Find(&rows).Error)

	return rows
}

func TestAdmissionRecoveryHandoffAtomicReleaseAndReclaim(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installRecoveryHandoff(t, db)

	now := time.Now()
	task := recoveryHandoffTask(t, f, now.Add(time.Minute))
	h, err := model.PrepareNativeRecoveryHandoff(t.Context(), db, task)
	require.NoError(t, err)
	require.NotNil(t, h)
	//nolint:staticcheck // Regression requires private authorization handles to serialize as an empty object.
	raw, err := json.Marshal(h)
	require.NoError(t, err)
	require.Equal(t, "{}", string(raw))
	released, err := h.ReleaseAndRecordExit(t.Context(), task, now, false)
	require.Error(t, err)
	require.False(t, released)
	accepted, err := h.Accept(t.Context())
	require.NoError(t, err)
	require.True(t, accepted)
	accepted, err = h.Accept(t.Context())
	require.NoError(t, err)
	require.False(t, accepted)
	// Extension must not create a second handoff for the SAME unique token.
	require.NoError(
		t,
		model.ExtendNativeRecovery(db, task.ID, task.RecoveryOwner, now.Add(11*time.Minute)),
	)
	extended, err := model.GetNativeTask(db, task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)
	duplicate, err := model.PrepareNativeRecoveryHandoff(t.Context(), db, extended)
	require.Error(t, err)
	require.Nil(t, duplicate)
	require.Len(t, nativeExecutionRows(t, db), 1)
	released, err = h.ReleaseAndRecordExit(t.Context(), task, now, false)
	require.NoError(t, err)
	require.True(t, released)
	row := recoveryHandoffRows(t, db)[0]
	require.Equal(t, "exited", row.State)
	execution := nativeExecutionRows(t, db)[0]
	require.Equal(t, "returned", execution.State)
	require.Equal(t, row.ExitedAtMs, execution.ReturnedAtMs)

	fresh, err := model.GetNativeTask(db, task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)
	require.Empty(t, fresh.RecoveryOwner)
	require.Zero(t, fresh.RecoveryUntil)
	require.Equal(t, now.Unix(), fresh.NextRecoveryAt)
	claims, err := model.ClaimNativeRecovery(db, "same-worker", now, 1)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	nextClaim := claims[0]
	require.NotEqual(t, task.RecoveryOwner, nextClaim.RecoveryOwner)
	// Simulated lost success response: retry cannot clear a subsequent claim.
	released, err = h.ReleaseAndRecordExit(t.Context(), &nextClaim, now, false)
	require.Error(t, err)
	require.False(t, released)
	require.Error(
		t,
		model.ExtendNativeRecovery(db, task.ID, task.RecoveryOwner, now.Add(12*time.Minute)),
	)
	next, err := model.PrepareNativeRecoveryHandoff(t.Context(), db, &nextClaim)
	require.NoError(t, err)
	accepted, err = next.Accept(t.Context())
	require.NoError(t, err)
	require.True(t, accepted)
	released, err = next.ReleaseAndRecordExit(t.Context(), &nextClaim, now.Add(time.Minute), true)
	require.NoError(t, err)
	require.True(t, released)
	require.Len(t, recoveryHandoffRows(t, db), 2)
	require.Equal(t, "open", runtimeUses(t, db)[0].State)

	for _, r := range nativeExecutionRows(t, db) {
		require.Equal(t, "unproven", r.ContinuationState)
	}
}

func TestAdmissionRecoveryHandoffScheduleSnapshot(t *testing.T) {
	for _, change := range []string{"status", "same-status-dispatch", "enqueued", "created", "scope", "upstream", "receipt"} {
		t.Run(change, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installRecoveryHandoff(t, db)
			task := recoveryHandoffTask(t, f, time.Now().Add(time.Minute))
			h, err := model.PrepareNativeRecoveryHandoff(t.Context(), db, task)
			require.NoError(t, err)
			_, err = h.Accept(t.Context())
			require.NoError(t, err)

			changes := map[string]any{}
			switch change {
			case "status":
				changes["status"] = "running"
			case "same-status-dispatch":
				changes["dispatch_owner"] = "next-dispatch"
				changes["dispatch_until"] = time.Now().Add(30 * time.Second)
			case "enqueued":
				changes["enqueued_at"] = time.Now()
			case "created":
				changes["created_at"] = time.Now().Add(time.Second)
			case "scope":
				changes["queue_scope"] = "different-scope"
			case "upstream":
				changes["upstream_id"] = "provider-next"
			case "receipt":
				changes["billing_receipt_json"] = `{"status":"pending"}`
			}

			require.NoError(
				t,
				db.Model(&model.NativeTask{}).Where("id = ?", task.ID).Updates(changes).Error,
			)
			released, err := h.ReleaseAndRecordExit(t.Context(), task, time.Now(), false)
			// A changed immutable queue scope fails identity validation rather than retry.
			if change == "scope" {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.False(t, released)
			require.Equal(t, "accepted", recoveryHandoffRows(t, db)[0].State)
			current, err := model.GetNativeTask(db, task.ID, task.GroupID, task.TokenID)
			require.NoError(t, err)
			released, err = h.ReleaseAndRecordExit(
				t.Context(),
				current,
				time.Now().Add(time.Minute),
				false,
			)
			require.NoError(t, err)
			require.True(t, released)
		})
	}
}

func TestAdmissionRecoveryHandoffInvalidationAndRollback(t *testing.T) {
	for _, mode := range []string{"owner", "expired", "shortened", "storage", "proof", "capability", "hash", "task-write", "exit-write", "return-write", "source-outage", "epoch"} {
		t.Run(mode, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installRecoveryHandoff(t, db)
			task := recoveryHandoffTask(t, f, time.Now().Add(time.Minute))
			h, err := model.PrepareNativeRecoveryHandoff(t.Context(), db, task)
			require.NoError(t, err)
			_, err = h.Accept(t.Context())
			require.NoError(t, err)

			switch mode {
			case "owner":
				require.NoError(t, db.Model(task).Update("recovery_owner", "new-owner").Error)
			case "expired":
				require.NoError(
					t,
					db.Model(task).
						Update("recovery_until", time.Now().Add(-time.Second).Unix()).
						Error,
				)
			case "shortened":
				require.NoError(
					t,
					db.Model(task).Update("recovery_until", task.RecoveryUntil-10).Error,
				)
			case "storage":
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionControl{}).
						Where("id=1").
						Update("storage_generation", "other").
						Error,
				)
			case "proof":
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionProof{}).
						Where("1=1").
						Update("revoked", true).
						Error,
				)
			case "capability":
				h = &model.NativeRecoveryHandoffHandle{}
			case "hash":
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionRecoveryHandoff{}).
						Where("1=1").
						Update("authorization_hash", strings.Repeat("a", 64)).
						Error,
				)
			case "source-outage":
				restore := model.ConfigureChannelAdmissionRuntime(f.store.Executor, nil)
				defer restore()
			case "epoch":
				restore := advanceNativeExecutionEpoch(t, f, 2, false)
				defer restore()
			default:
				table := map[string]string{"task-write": "native_tasks", "exit-write": "channel_admission_recovery_handoffs", "return-write": "channel_admission_task_executions"}[mode]

				require.NoError(
					t,
					db.Callback().
						Update().
						After("gorm:update").
						Register("test:release-failure", func(tx *gorm.DB) {
							if tx.Statement.Table == table {
								_ = tx.AddError(errors.New("synthetic write failure"))
							}
						}),
				)
				defer func() {
					if err := db.
						Callback().Update().
						Remove("test:release-failure"); err != nil {
						t.Error(err)
					}
				}()
			}

			before, err := model.GetNativeTask(db, task.ID, task.GroupID, task.TokenID)
			require.NoError(t, err)

			released, err := h.ReleaseAndRecordExit(
				t.Context(),
				task,
				time.Now().Add(time.Minute),
				false,
			)
			if mode == "source-outage" || mode == "epoch" {
				require.NoError(t, err)
				require.True(t, released)
			} else {
				require.Error(t, err)
				require.False(t, released)

				after, e := model.GetNativeTask(db, task.ID, task.GroupID, task.TokenID)
				require.NoError(t, e)
				require.Equal(t, before.RecoveryOwner, after.RecoveryOwner)
				require.Equal(t, before.RecoveryUntil, after.RecoveryUntil)
				require.Equal(t, before.NextRecoveryAt, after.NextRecoveryAt)
				require.Equal(t, "accepted", recoveryHandoffRows(t, db)[0].State)
				require.Equal(t, "active", nativeExecutionRows(t, db)[0].State)
			}

			require.Equal(t, "open", runtimeUses(t, db)[0].State)
		})
	}
}

func TestAdmissionRecoveryHandoffExpiryDuringWrite(t *testing.T) {
	for _, phase := range []string{"offer", "accept", "release", "exit", "return"} {
		t.Run(phase, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installRecoveryHandoff(t, db)

			deadline := time.Now().Add(2 * time.Second).Truncate(time.Second)
			task := recoveryHandoffTask(t, f, deadline)

			var (
				h   *model.NativeRecoveryHandoffHandle
				err error
			)
			if phase != "offer" {
				h, err = model.PrepareNativeRecoveryHandoff(t.Context(), db, task)
				require.NoError(t, err)
			}

			if phase != "offer" && phase != "accept" {
				_, err = h.Accept(t.Context())
				require.NoError(t, err)
			}

			table := map[string]string{"offer": "channel_admission_recovery_handoffs", "accept": "channel_admission_recovery_handoffs", "release": "native_tasks", "exit": "channel_admission_recovery_handoffs", "return": "channel_admission_task_executions"}[phase]

			pause := func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					if delay := time.Until(deadline.Add(30 * time.Millisecond)); delay > 0 {
						time.Sleep(delay)
					}
				}
			}
			if phase == "offer" {
				require.NoError(
					t,
					db.Callback().Create().After("gorm:create").Register("test:expire", pause),
				)
				defer func() {
					if err := db.
						Callback().Create().
						Remove("test:expire"); err !=
						nil {
						t.Error(err)
					}
				}()

				_, err = model.PrepareNativeRecoveryHandoff(t.Context(), db, task)
			} else {
				require.NoError(
					t,
					db.Callback().Update().After("gorm:update").Register("test:expire", pause),
				)
				defer func() {
					if err := db.
						Callback().Update().
						Remove("test:expire"); err !=
						nil {
						t.Error(err)
					}
				}()

				if phase == "accept" {
					_, err = h.Accept(t.Context())
				} else {
					_, err = h.ReleaseAndRecordExit(
						t.Context(),
						task,
						time.Now().Add(time.Minute),
						false,
					)
				}
			}

			require.Error(t, err)

			fresh, e := model.GetNativeTask(db, task.ID, task.GroupID, task.TokenID)
			require.NoError(t, e)
			require.Equal(t, task.RecoveryOwner, fresh.RecoveryOwner)
			require.Equal(t, task.RecoveryUntil, fresh.RecoveryUntil)
			require.Equal(t, task.NextRecoveryAt, fresh.NextRecoveryAt)

			if phase == "offer" {
				require.Empty(t, recoveryHandoffRows(t, db))
				require.Empty(t, nativeExecutionRows(t, db))
			} else {
				want := "accepted"
				if phase == "accept" {
					want = "offered"
				}

				require.Equal(t, want, recoveryHandoffRows(t, db)[0].State)
				require.Equal(t, "active", nativeExecutionRows(t, db)[0].State)
			}
		})
	}
}

func TestAdmissionRecoveryHandoffPartialStorageAndAtomicOffer(t *testing.T) {
	for _, mode := range []string{"absent", "missing-table", "missing-protocol", "wrong-version", "missing-column", "offer-write-fails", "valid"} {
		t.Run(mode, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installNativeExecution(t, db)

			task := recoveryHandoffTask(t, f, time.Now().Add(time.Minute))
			if mode != "absent" {
				installRecoveryHandoff(t, db)
			}

			switch mode {
			case "missing-table":
				require.NoError(
					t,
					db.Migrator().DropTable(&model.ChannelAdmissionRecoveryHandoff{}),
				)
			case "missing-protocol":
				require.NoError(
					t,
					db.Where("id = ?", 4).Delete(&model.ChannelAdmissionExecutionProtocol{}).Error,
				)
			case "wrong-version":
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionExecutionProtocol{}).
						Where("id = ?", 4).
						Update("version", 99).
						Error,
				)
			case "missing-column":
				require.NoError(
					t,
					db.Exec(
						"ALTER TABLE channel_admission_recovery_handoffs DROP COLUMN accepted_at_ms",
					).Error,
				)
			case "offer-write-fails":
				require.NoError(
					t,
					db.Callback().
						Create().
						After("gorm:create").
						Register("test:offer-failure", func(tx *gorm.DB) {
							if tx.Statement.Table == "channel_admission_recovery_handoffs" {
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

			h, err := model.PrepareNativeRecoveryHandoff(t.Context(), db, task)
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

func TestAdmissionRecoveryHandoffConcurrentAccept(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installRecoveryHandoff(t, db)
	task := recoveryHandoffTask(t, f, time.Now().Add(time.Minute))
	h, err := model.PrepareNativeRecoveryHandoff(t.Context(), db, task)
	require.NoError(t, err)

	type result struct {
		accepted bool
		err      error
	}

	results := make(chan result, 2)
	for range 2 {
		go func() { a, e := h.Accept(context.Background()); results <- result{a, e} }()
	}

	count := 0
	for range 2 {
		r := <-results
		require.NoError(t, r.err)

		if r.accepted {
			count++
		}
	}

	require.Equal(t, 1, count)
}

func TestAdmissionRecoveryHandoffOrphanProtocolRejected(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installExecutionJournal(t, db)
	require.NoError(t, db.Create(&model.ChannelAdmissionExecutionProtocol{ID: 4, Version: 1}).Error)
	task := recoveryHandoffTask(t, f, time.Now().Add(time.Minute))
	h, err := model.PrepareNativeRecoveryHandoff(t.Context(), db, task)
	require.Error(t, err)
	require.Nil(t, h)
}
