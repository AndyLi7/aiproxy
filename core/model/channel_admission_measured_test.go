package model_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func installMeasured(t *testing.T, db *gorm.DB) {
	t.Helper()
	installExecutionJournal(t, db)
	raw, e := os.ReadFile("../migrations/channel-measured-handoff-v1/" + db.Name() + ".sql")
	require.NoError(t, e)

	var lines []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}

	parts := strings.Split(strings.TrimSpace(strings.Join(lines, "\n")), ";")
	for range 2 {
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			for _, p := range parts[1 : len(parts)-2] {
				if e := tx.Exec(p).Error; e != nil {
					return e
				}
			}

			return nil
		}))
	}
}

func measuredRows(t *testing.T, db *gorm.DB) []model.ChannelAdmissionMeasuredClaim {
	t.Helper()

	var r []model.ChannelAdmissionMeasuredClaim
	require.NoError(t, db.Order("offered_at_ms,id").Find(&r).Error)

	return r
}

func measuredSource(
	t *testing.T,
	f *admissionFixture,
) (*model.MeasuredRelayIntent, model.Price, *model.Log, *model.AsyncUsageInfo) {
	t.Helper()

	db := f.store.DB
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	at := time.Now().UTC().Truncate(time.Millisecond)
	price := model.Price{
		OutputPrice:     .3,
		OutputPriceUnit: 1,
		ImageBilling:    &model.ImageBillingPolicy{Version: 1, Scenario: "generation"},
	}
	intent := model.NewMeasuredRelayIntent(price, at, false)

	var ch model.Channel
	require.NoError(t, db.First(&ch, 1).Error)
	_, ex, e := model.AdmitRelayExecution(
		t.Context(),
		&ch,
		model.RelayAdmissionScope{
			GroupID:            "g",
			TokenID:            1,
			BillingOperationID: "measured-op",
			RequestID:          "measured-request",
			Model:              "image",
			Mode:               3,
		},
		intent,
	)
	require.NoError(t, e)
	require.NotNil(t, ex)
	require.NoError(t, ex.RecordReturn(t.Context(), model.RelayExecutionReturned))

	micros := int64(300000)
	u := &model.AsyncUsageInfo{
		GroupID:            "g",
		TokenID:            1,
		BillingOperationID: "measured-op",
		RequestID:          "measured-request",
		Model:              "image",
		Mode:               3,
		ChannelID:          1,
		RequestAt:          at,
		Price:              price,
		Amount: model.Amount{
			UsedAmount:         .3,
			ImageBillingResult: &model.ImageBillingResult{State: "complete", AmountMicros: &micros},
		},
	}
	entry := &model.Log{
		GroupID:            "g",
		TokenID:            1,
		BillingOperationID: "measured-op",
		RequestID:          "measured-request",
		Model:              "image",
		Mode:               3,
		ChannelID:          1,
		RequestAt:          at,
	}

	return intent, price, entry, u
}

func persistMeasured(
	t *testing.T,
	f *admissionFixture,
) (*model.MeasuredRelayIntent, *model.AsyncUsageInfo) {
	t.Helper()
	i, p, l, u := measuredSource(t, f)
	require.NoError(t, model.CreateMeasuredImageSettlementOwned(t.Context(), i, p, l, u))
	require.NoError(t, model.RecordMeasuredProducerReturn(t.Context(), i))

	return i, u
}

func claimMeasured(
	t *testing.T,
	db *gorm.DB,
	u *model.AsyncUsageInfo,
	token string,
	d time.Duration,
) {
	t.Helper()
	require.NoError(
		t,
		db.Model(&model.AsyncUsageInfo{}).
			Where("id=?", u.ID).
			Update("next_poll_at", time.Now().Add(-time.Second)).
			Error,
	)
	require.NoError(t, db.First(u, u.ID).Error)
	won, e := model.TryClaimAsyncUsageInfoContext(
		t.Context(),
		u,
		token,
		time.Now().Add(d),
		time.Now(),
	)
	require.NoError(t, e)
	require.True(t, won)
}

func TestMeasuredHandoffSource(t *testing.T) {
	for _, scenario := range []string{"normal", "evidence", "price", "operation", "channel", "receipt-failure", "return-failure", "late", "partial", "idempotent", "copied-usage", "wrong-version", "reuse-intent"} {
		t.Run(scenario, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)

			db := f.store.DB
			if scenario != "late" {
				installMeasured(t, db)
			} else {
				installExecutionJournal(t, db)
			}

			i, p, l, u := measuredSource(t, f)
			if scenario == "late" {
				installMeasured(t, db)
			}

			if scenario == "wrong-version" {
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionExecutionProtocol{}).
						Where("id=9").
						Update("version", 2).
						Error,
				)
			}

			if scenario == "reuse-intent" {
				var ch model.Channel
				require.NoError(t, db.First(&ch, 1).Error)
				_, _, e := model.AdmitRelayExecution(
					t.Context(),
					&ch,
					model.RelayAdmissionScope{
						GroupID:            "g",
						TokenID:            1,
						BillingOperationID: "measured-op",
						RequestID:          "measured-request",
						Model:              "image",
						Mode:               3,
					},
					i,
				)
				require.Error(t, e)

				return
			}

			if scenario == "partial" {
				require.NoError(
					t,
					db.Exec(
						"ALTER TABLE channel_admission_measured_producers DROP COLUMN frozen_hash",
					).Error,
				)
			}

			if scenario == "price" {
				p.OutputPrice = 9
			}

			if scenario == "operation" {
				u.BillingOperationID = "other"
			}

			if scenario == "channel" {
				u.ChannelID = 2
			}

			if scenario == "receipt-failure" {
				require.NoError(
					t,
					db.Callback().
						Create().
						Before("gorm:create").
						Register("measured:fail", func(tx *gorm.DB) {
							if tx.Statement.Table == "async_usage_infos" {
								_ = tx.AddError(errors.New("synthetic outbox failure"))
							}
						}),
				)
			}

			var e error
			if scenario == "evidence" {
				e = model.CreateMeasuredImageAttemptEvidenceOwned(t.Context(), i, p, u)
			} else {
				e = model.CreateMeasuredImageSettlementOwned(t.Context(), i, p, l, u)
			}

			switch scenario {
			case "price", "operation", "channel", "receipt-failure", "partial", "wrong-version":
				require.Error(t, e)

				var n int64
				require.NoError(t, db.Model(&model.Log{}).Count(&n).Error)
				require.Zero(t, n)

				return
			}

			require.NoError(t, e)

			if scenario == "idempotent" {
				require.NoError(
					t,
					model.CreateMeasuredImageSettlementOwned(t.Context(), i, p, l, u),
				)

				var n int64
				require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Count(&n).Error)
				require.EqualValues(t, 1, n)
			}

			if scenario == "return-failure" {
				require.NoError(
					t,
					db.Callback().
						Update().
						Before("gorm:update").
						Register("measured:return", func(tx *gorm.DB) {
							if tx.Statement.Table == "channel_admission_measured_producers" {
								_ = tx.AddError(errors.New("synthetic return failure"))
							}
						}),
				)
			}

			e = model.RecordMeasuredProducerReturn(t.Context(), i)

			var rows []model.ChannelAdmissionMeasuredProducer
			require.NoError(t, db.Find(&rows).Error)

			if scenario == "late" {
				require.NoError(t, e)
				require.Empty(t, rows)
				return
			}

			require.Len(t, rows, 1)

			if scenario == "return-failure" {
				require.Error(t, e)
				require.Equal(t, "active", rows[0].State)
				return
			}

			require.NoError(t, e)
			require.Equal(t, "returned", rows[0].State)

			if scenario == "evidence" {
				require.Zero(t, u.LogID)
				require.NotEqual(t, model.AsyncUsageStatusPending, u.Status)
				return
			}

			if scenario == "copied-usage" {
				claimMeasured(t, db, u, "owner", time.Minute)

				var snapshotCopy model.AsyncUsageInfo
				require.NoError(t, db.First(&snapshotCopy, u.ID).Error)
				ok, e := model.AcceptImagePoll(t.Context(), &snapshotCopy)
				require.Error(t, e)
				require.False(t, ok)
				require.Error(t, model.ValidateImageAccountingClaim(t.Context(), &snapshotCopy))
				_, e = model.RenewImagePollClaim(
					t.Context(),
					&snapshotCopy,
					time.Now().Add(2*time.Minute),
				)
				require.Error(t, e)
			}
		})
	}
}

func TestMeasuredHandoffConsumer(t *testing.T) {
	for _, scenario := range []string{"complete", "park", "fail", "renew", "expired", "replaced", "new-owner", "wrong-frozen", "duplicate", "parent-cancel", "log-channel"} {
		t.Run(scenario, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installMeasured(t, db)
			_, u := persistMeasured(t, f)
			claimMeasured(t, db, u, "owner", time.Minute)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			ok, e := model.AcceptImagePoll(ctx, u)
			require.NoError(t, e)
			require.True(t, ok)

			before := measuredRows(t, db)[0]
			if scenario == "duplicate" {
				ok, e = model.AcceptImagePoll(ctx, u)
				require.NoError(t, e)
				require.False(t, ok)
				return
			}

			if scenario == "renew" {
				ok, e = model.RenewImagePollClaim(ctx, u, time.Now().Add(2*time.Minute))
				require.NoError(t, e)
				require.True(t, ok)
			}

			if scenario == "expired" || scenario == "new-owner" {
				require.NoError(
					t,
					db.Model(&model.AsyncUsageInfo{}).
						Where("id=?", u.ID).
						Update("next_poll_at", time.Now().Add(-time.Second)).
						Error,
				)
			}

			if scenario == "replaced" {
				require.NoError(
					t,
					db.Model(&model.AsyncUsageInfo{}).
						Where("id=?", u.ID).
						Update("processing_token", "another").
						Error,
				)
			}

			if scenario == "wrong-frozen" {
				u.Price.OutputPrice = 9
			}

			if scenario == "log-channel" {
				require.NoError(
					t,
					db.Model(&model.Log{}).Where("id=?", u.LogID).Update("channel_id", 2).Error,
				)
			}

			if scenario == "parent-cancel" {
				cancel()
			}

			if scenario == "new-owner" {
				old := *u

				var fresh model.AsyncUsageInfo
				require.NoError(t, db.First(&fresh, u.ID).Error)
				claimMeasured(t, db, &fresh, "new-owner", time.Minute)
				ok, e = model.AcceptImagePoll(ctx, &fresh)
				require.NoError(t, e)
				require.True(t, ok)
				require.Error(t, model.RecordImagePollReturn(ctx, &old))
				require.Equal(t, before, measuredRows(t, db)[0])

				u = &fresh
			}

			switch scenario {
			case "park":
				e = model.ParkMeasuredImageUsage(u)
			case "fail":
				u.Error = "synthetic failure"
				_, e = model.FailClaimedAsyncUsageInfo(u)
			default:
				_, e = model.CompleteClaimedAsyncUsageInfo(u, u.Usage, u.UsageContext, u.Amount)
			}

			switch scenario {
			case "expired", "replaced", "wrong-frozen", "parent-cancel", "log-channel":
				require.Error(t, e)
				require.Equal(t, before, measuredRows(t, db)[0])
				return
			}

			require.NoError(t, e)
			require.NoError(t, model.RecordImagePollReturn(ctx, u))

			var row model.ChannelAdmissionMeasuredClaim
			require.NoError(t, db.Where("state=?", "exited").First(&row).Error)
			require.NotEmpty(t, row.ReleaseHash)

			var use model.ChannelAdmissionUse
			require.NoError(t, db.First(&use).Error)
			require.Equal(t, "open", use.State)
		})
	}
}

func TestMeasuredHandoffCoveredCannotUseLegacy(t *testing.T) {
	for _, mode := range []string{"nil-owned", "legacy-final", "legacy-evidence"} {
		t.Run(mode, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			installMeasured(t, f.store.DB)
			_, p, l, u := measuredSource(t, f)

			var err error
			switch mode {
			case "nil-owned":
				err = model.CreateMeasuredImageSettlementOwned(t.Context(), nil, p, l, u)
			case "legacy-final":
				err = model.CreateMeasuredImageSettlement(l, u)
			case "legacy-evidence":
				err = model.CreateMeasuredImageAttemptEvidence(u)
			}

			require.Error(t, err)

			var n int64
			require.NoError(t, f.store.DB.Model(&model.AsyncUsageInfo{}).Count(&n).Error)
			require.Zero(t, n)
		})
	}
}
