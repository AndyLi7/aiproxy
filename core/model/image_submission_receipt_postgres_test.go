package model_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func waitSubmissionDeadline(t *testing.T, task *model.ImageTask) {
	t.Helper()

	if delay := time.Until(
		task.CreatedAt.Add(model.AsyncGenerationDeadline).Add(15 * time.Millisecond),
	); delay > 0 {
		time.Sleep(delay)
	}
}

func TestPostgresImageSubmissionTerminalSerialization(t *testing.T) {
	for _, receipt := range []string{"legacy-accept", "owned-accept", "owned-complete"} {
		for _, order := range []string{"terminal-first", "receipt-first", "receipt-read-then-terminal"} {
			t.Run(receipt+"/"+order, func(t *testing.T) {
				db := queueLockPostgres(t)
				task, info := submissionReceiptFixture(
					t,
					db,
					model.AsyncGenerationDeadline-time.Second,
				)
				receive := func() error {
					if receipt == "legacy-accept" {
						return model.AcceptImageTask(task.ID, "upstream")
					}

					if receipt == "owned-accept" {
						return model.AcceptSubmittedImageTask(t.Context(), task, "upstream")
					}

					return model.CompleteSubmittedImageTask(
						t.Context(),
						task,
						[]model.ImageOutput{{URL: "https://example.invalid/image"}},
					)
				}

				expire := func() (bool, error) { return model.ExpireUnconfirmedImageSubmission(task.ID, time.Now()) }
				switch order {
				case "terminal-first":
					waitSubmissionDeadline(t, task)

					won, e := expire()
					require.NoError(t, e)
					require.True(t, won)
					require.Error(t, receive())
				case "receipt-first":
					require.NoError(t, receive())
					waitSubmissionDeadline(t, task)

					won, e := expire()
					require.NoError(t, e)
					require.False(t, won)
				default:
					read := make(chan struct{})
					resume := make(chan struct{})
					received := make(chan error, 1)
					finished := make(chan struct{})

					var resumeOnce sync.Once

					release := func() { resumeOnce.Do(func() { close(resume) }) }
					defer func() {
						release()

						select {
						case <-finished:
						case <-time.After(10 * time.Second):
							t.Error("receipt did not join")
						}
					}()

					var seen atomic.Bool
					require.NoError(
						t,
						db.Callback().
							Query().
							After("gorm:query").
							Register("receipt:read-barrier", func(tx *gorm.DB) {
								if tx.Statement.Table != "image_tasks" ||
									!seen.CompareAndSwap(false, true) {
									return
								}

								close(read)

								select {
								case <-resume:
								case <-time.After(8 * time.Second):
									_ = tx.AddError(context.DeadlineExceeded)
								}
							}),
					)

					go func() { defer close(finished); received <- receive() }()

					select {
					case <-read:
					case e := <-received:
						t.Fatalf("receipt returned before read: %v", e)
					case <-time.After(5 * time.Second):
						release()

						receiptErr := <-received
						t.Fatalf("no read; receipt result: %v", receiptErr)
					}

					waitSubmissionDeadline(t, task)

					type result struct {
						won bool
						err error
					}

					expired := make(chan result, 1)
					go func() { w, e := expire(); expired <- result{w, e} }()

					var app string
					require.NoError(
						t,
						db.Raw("SELECT current_setting('application_name')").Scan(&app).Error,
					)
					require.Eventually(t, func() bool {
						var count int64

						err := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).
							Scan(&count).
							Error

						return err == nil && count > 0
					}, 2*time.Second, 5*time.Millisecond, "terminal writer must wait on receipt's task lock")
					release()
					require.NoError(t, <-received)

					outcome := <-expired
					require.NoError(t, outcome.err)
					require.False(t, outcome.won)
				}

				var (
					current model.ImageTask
					usage   model.AsyncUsageInfo
					entry   model.Log
				)

				require.NoError(t, db.First(&current, "id=?", task.ID).Error)
				require.NoError(t, db.First(&usage, info.ID).Error)
				require.NoError(t, db.First(&entry, info.LogID).Error)

				if order == "terminal-first" {
					require.Equal(t, "failed", current.Status)
					require.Empty(t, current.UpstreamID)
					require.Equal(t, model.AsyncUsageStatusFailed, usage.Status)
					require.Equal(t, model.AsyncUsageStatusFailed, entry.AsyncUsageStatus)
				} else {
					want := "queued"
					if receipt == "owned-complete" {
						want = "completed"
					}

					require.Equal(t, want, current.Status)
					require.Nil(t, current.Error)
					require.Equal(t, model.AsyncUsageStatusPending, usage.Status)
					require.Equal(t, model.AsyncUsageStatusPending, entry.AsyncUsageStatus)
				}
			})
		}
	}
}

func TestPostgresImageSubmissionFinalLogLock(t *testing.T) {
	for _, receipt := range []string{"accept", "complete"} {
		for _, outcome := range []string{"live", "changed", "cancelled"} {
			t.Run(receipt+"/"+outcome, func(t *testing.T) {
				db := queueLockPostgres(t)
				task, info := submissionReceiptFixture(t, db)
				pool, e := db.DB()
				require.NoError(t, e)
				holder, e := pool.BeginTx(t.Context(), nil)
				require.NoError(t, e)

				defer func() {
					if err := holder.
						Rollback(); err !=
						nil &&
						!errors.Is(err,
							sql.ErrTxDone,
						) {
						t.Error(err)
					}
				}()

				var id int
				require.NoError(
					t,
					holder.QueryRowContext(t.Context(), "SELECT id FROM logs WHERE id=$1 FOR UPDATE", info.LogID).
						Scan(&id),
				)

				var (
					before      model.ImageTask
					beforeUsage model.AsyncUsageInfo
				)

				require.NoError(t, db.First(&before, "id=?", task.ID).Error)
				require.NoError(t, db.First(&beforeUsage, info.ID).Error)

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				done := make(chan error, 1)
				go func() {
					if receipt == "accept" {
						done <- model.AcceptSubmittedImageTask(ctx, task, "upstream")
					} else {
						done <- model.CompleteSubmittedImageTask(ctx, task, []model.ImageOutput{{URL: "https://example.invalid/image"}})
					}
				}()

				var app string
				require.NoError(
					t,
					db.Raw("SELECT current_setting('application_name')").Scan(&app).Error,
				)
				require.Eventually(t, func() bool {
					var count int64

					err := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).
						Scan(&count).
						Error

					return err == nil && count > 0
				}, 2*time.Second, 5*time.Millisecond)

				if outcome == "changed" {
					_, e = holder.ExecContext(
						t.Context(),
						"UPDATE logs SET billing_operation_id='changed' WHERE id=$1",
						info.LogID,
					)
					require.NoError(t, e)
				}

				if outcome == "cancelled" {
					cancel()

					select {
					case e = <-done:
						require.Error(t, e)
					case <-time.After(time.Second):
						t.Fatal("cancelled receipt blocked")
					}
				}

				require.NoError(t, holder.Commit())

				if outcome != "cancelled" {
					e = <-done
					if outcome == "live" {
						require.NoError(t, e)
					} else {
						require.Error(t, e)
					}
				}

				if outcome != "live" {
					var (
						got   model.ImageTask
						usage model.AsyncUsageInfo
					)

					require.NoError(t, db.First(&got, "id=?", task.ID).Error)
					require.NoError(t, db.First(&usage, info.ID).Error)
					require.Equal(t, before, got)
					require.Equal(t, beforeUsage, usage)
				}
			})
		}
	}
}
