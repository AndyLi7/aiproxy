package model_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

//nolint:gocyclo // The lock matrix deliberately retains each scenario setup and its corresponding post-lock assertions together.
func TestPostgresMeasuredHandoffLocks(t *testing.T) {
	for _, action := range []string{"producer", "accept", "log", "return", "renew"} {
		for _, outcome := range []string{"live", "changed", "cancelled", "expired"} {
			if action == "producer" && outcome == "expired" {
				continue
			}

			t.Run(action+"/"+outcome, func(t *testing.T) {
				db := queueLockPostgres(t)
				f := runtimeAdmissionOnFixture(t, admissionFixtureOn(t, db, ""))
				installMeasured(t, db)
				i, p, l, u := measuredSource(t, f)

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				if action != "producer" {
					require.NoError(
						t,
						model.CreateMeasuredImageSettlementOwned(t.Context(), i, p, l, u),
					)
					require.NoError(t, model.RecordMeasuredProducerReturn(t.Context(), i))
					claimMeasured(t, db, u, "owner", 3*time.Second)
				}

				if action == "log" || action == "return" || action == "renew" {
					ok, e := model.AcceptImagePoll(ctx, u)
					require.NoError(t, e)
					require.True(t, ok)
				}

				if action == "return" {
					_, e := model.CompleteClaimedAsyncUsageInfo(
						u,
						u.Usage,
						u.UsageContext,
						u.Amount,
					)
					require.NoError(t, e)
				}

				var before model.AsyncUsageInfo
				if u.ID != 0 {
					require.NoError(t, db.First(&before, u.ID).Error)
				}

				var source model.ChannelAdmissionMeasuredProducer
				require.NoError(t, db.First(&source).Error)

				var claim model.ChannelAdmissionMeasuredClaim
				if action != "producer" {
					require.NoError(t, db.First(&claim).Error)
				}

				pool, e := db.DB()
				require.NoError(t, e)
				holder, e := pool.BeginTx(t.Context(), nil)
				require.NoError(t, e)

				defer func() {
					if err := holder.
						Rollback(); err !=
						nil &&
						!errors.Is(err, sql.
							ErrTxDone) {
						t.Error(err)
					}
				}()

				switch action {
				case "producer":
					var id string
					require.NoError(
						t,
						holder.QueryRowContext(t.Context(), "SELECT id FROM channel_admission_measured_producers WHERE id=$1 FOR UPDATE", source.ID).
							Scan(&id),
					)
				case "log":
					var id int
					require.NoError(
						t,
						holder.QueryRowContext(t.Context(), "SELECT id FROM logs WHERE id=$1 FOR UPDATE", u.LogID).
							Scan(&id),
					)
				default:
					var id int
					require.NoError(
						t,
						holder.QueryRowContext(t.Context(), "SELECT id FROM channel_admission_execution_protocols WHERE id=9 FOR UPDATE").
							Scan(&id),
					)
				}

				done := make(chan error, 1)
				go func() {
					switch action {
					case "producer":
						done <- model.CreateMeasuredImageSettlementOwned(ctx, i, p, l, u)
					case "accept":
						_, e := model.AcceptImagePoll(ctx, u)
						done <- e
					case "log":
						_, e := model.CompleteClaimedAsyncUsageInfo(
							u,
							u.Usage,
							u.UsageContext,
							u.Amount,
						)
						done <- e
					case "return":
						done <- model.RecordImagePollReturn(ctx, u)
					case "renew":
						won, e := model.RenewImagePollClaim(ctx, u, time.Now().Add(4*time.Minute))
						if !won && e == nil {
							e = errors.New("original lease refused")
						}

						done <- e
					}
				}()

				var app string
				require.NoError(
					t,
					db.Raw("SELECT current_setting('application_name')").Scan(&app).Error,
				)
				require.Eventually(t, func() bool {
					var n int64

					e := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event_type='Lock'", app).
						Scan(&n).
						Error

					return e == nil && n > 0
				}, 2*time.Second, 5*time.Millisecond)

				if outcome == "changed" {
					switch action {
					case "producer":
						_, e = holder.ExecContext(
							t.Context(),
							"UPDATE channel_admission_measured_producers SET authorization_hash='changed' WHERE id=$1",
							source.ID,
						)
					case "log":
						_, e = holder.ExecContext(
							t.Context(),
							"UPDATE logs SET billing_operation_id='changed' WHERE id=$1",
							u.LogID,
						)
					default:
						_, e = holder.ExecContext(
							t.Context(),
							"UPDATE channel_admission_execution_protocols SET version=2 WHERE id=9",
						)
					}

					require.NoError(t, e)
				}

				if outcome == "expired" {
					if delay := time.Until(
						before.NextPollAt.Add(20 * time.Millisecond),
					); delay > 0 {
						time.Sleep(delay)
					}
				}

				if outcome == "cancelled" {
					cancel()

					select {
					case e = <-done:
						require.Error(t, e)
					case <-time.After(time.Second):
						t.Fatal("cancel did not unblock source/claim")
					}

					require.NoError(t, holder.Rollback())
				} else {
					require.NoError(t, holder.Commit())

					e = <-done
					if outcome == "live" {
						require.NoError(t, e)
					} else {
						require.Error(t, e)
					}
				}

				if outcome != "live" {
					if action == "producer" {
						var count int64
						require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Count(&count).Error)
						require.Zero(t, count)
					} else {
						var got model.AsyncUsageInfo
						require.NoError(t, db.First(&got, u.ID).Error)
						require.Equal(t, before, got)

						var gotClaim model.ChannelAdmissionMeasuredClaim
						require.NoError(t, db.First(&gotClaim, "id=?", claim.ID).Error)
						require.Equal(t, claim, gotClaim)
					}
				}
			})
		}
	}
}
