package model_test

import (
	"context"
	"errors"
	"math"
	"strconv"
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func cutoverUnchanged(
	t *testing.T,
	f *admissionFixture,
	req model.ChannelMigrationRequest,
	before string,
	rev int64,
) {
	t.Helper()

	db := f.store.DB
	require.Equal(t, before, migrationConfig(t, db))
	require.Equal(t, model.ChannelStatusDisabled, migrationStatus(t, db, req.Seal.To.ChannelID))

	var p model.ChannelMigrationPrepared
	require.NoError(t, db.First(&p, "operation_id=?", req.Seal.OperationID).Error)
	require.Equal(t, "prepared", p.State)
	require.Zero(t, p.SealRevision)
	require.Zero(t, p.CommitRevision)

	var c model.ChannelAdmissionControl
	require.NoError(t, db.First(&c, 1).Error)
	require.Equal(t, rev, c.Revision)

	var n int64
	require.NoError(t, db.Model(&model.ChannelAdmissionBarrier{}).Count(&n).Error)
	require.Zero(t, n)
	require.NoError(t, db.Model(&model.ChannelAdmissionFamily{}).Count(&n).Error)
	require.Zero(t, n)
}

func TestAtomicCutover(t *testing.T) {
	for _, to := range []int{2, 3} {
		t.Run(strconv.Itoa(to), func(t *testing.T) {
			f := newAdmissionFixture(t)
			req, _ := migrationFixture(t, f, to)
			ctx := context.Background()
			_, e := f.store.PrepareMigration(ctx, req)
			require.NoError(t, e)
			got, e := f.store.CutoverPrepared(ctx, req)
			require.NoError(t, e)
			require.Equal(t, "committed", got.State)
			require.EqualValues(t, 1, got.SealRevision)
			require.EqualValues(t, 2, got.CommitRevision)

			raw := migrationConfig(t, f.store.DB)
			for _, v := range []string{"9007199254740993123456789", "1.230000000000000000009", "123456789012345678901", "987654321098765432109", `"encryptionVersion":7`} {
				require.Contains(t, raw, v)
			}

			require.Equal(t, model.ChannelStatusEnabled, migrationStatus(t, f.store.DB, to))
			again, e := f.store.CutoverPrepared(ctx, req)
			require.NoError(t, e)
			require.Equal(t, got, again)

			read, e := f.store.ReadPreparedMigration(ctx, req.Seal.OperationID)
			require.NoError(t, e)
			require.Equal(t, got, read)
			require.Equal(
				t,
				"ready",
				f.store.ReadPreparedMigrationFacts(ctx, req.Seal.OperationID).State,
			)
			changed := req
			changed.FromCredentialID = "wrong"
			_, e = f.store.CutoverPrepared(ctx, changed)
			require.Error(t, e)
			_, e = f.store.Seal(ctx, f.seal("successor", got.CommitRevision, to, 5, ""))
			require.NoError(t, e)
			_, e = f.store.CutoverPrepared(ctx, req)
			require.Error(t, e)
		})
	}
}

func TestAtomicCutoverShadowResource(t *testing.T) {
	f := newAdmissionFixture(t)
	_, _, req := lifecycleFixture(t, f)
	_, e := f.store.PrepareMigration(t.Context(), req)
	require.NoError(t, e)
	got, e := f.store.CutoverPrepared(t.Context(), req)
	require.NoError(t, e)
	require.NotEmpty(t, got.ShadowCheckpointHash)
	require.Equal(
		t,
		"ready",
		f.store.ReadPreparedMigrationFacts(t.Context(), req.Seal.OperationID).State,
	)
}

func TestAtomicCutoverRollback(t *testing.T) {
	for _, at := range []string{"pin", "enable", "receipt", "final-proof-revoked", "final-probe-revoked"} {
		t.Run(at, func(t *testing.T) {
			f := newAdmissionFixture(t)
			req, _ := migrationFixture(t, f, 3)
			db := f.store.DB
			_, e := f.store.PrepareMigration(t.Context(), req)
			require.NoError(t, e)
			before := migrationConfig(t, db)
			require.NoError(
				t,
				db.Callback().
					Update().
					After("gorm:update").
					Register("cutover:fail", func(tx *gorm.DB) {
						if (at == "pin" && tx.Statement.Table == "model_configs") ||
							(at == "enable" && tx.Statement.Table == "channels") {
							_ = tx.AddError(errors.New("synthetic commit write failure"))
						}

						if p, ok := tx.Statement.Dest.(*model.ChannelMigrationPrepared); ok &&
							p.State == "committed" {
							if at == "final-proof-revoked" {
								_ = tx.AddError(
									tx.Session(&gorm.Session{NewDB: true}).
										Model(&model.ChannelAdmissionProof{}).
										Where("id=?", f.proof.ProofID).
										Update("revoked", true).
										Error,
								)
							}

							if at == "final-probe-revoked" {
								_ = tx.AddError(
									tx.Session(&gorm.Session{NewDB: true}).
										Model(&model.ChannelMigrationProbeRecord{}).
										Where("evidence_hash=?", p.ProbeHash).
										Update("revoked", true).
										Error,
								)
							}
						}

						if at == "receipt" {
							if p, ok := tx.Statement.Dest.(*model.ChannelMigrationPrepared); ok &&
								p.State == "committed" {
								_ = tx.AddError(errors.New("synthetic receipt failure"))
							}
						}
					}),
			)
			_, e = f.store.CutoverPrepared(t.Context(), req)
			require.Error(t, e)
			cutoverUnchanged(t, f, req, before, 0)
		})
	}
}

func TestAtomicCutoverRefusesHistoricalSealAndReverse(t *testing.T) {
	for _, state := range []string{"sealed", "reverse"} {
		t.Run(state, func(t *testing.T) {
			f := newAdmissionFixture(t)
			req, _ := migrationFixture(t, f, 3)
			_, e := f.store.PrepareMigration(t.Context(), req)
			require.NoError(t, e)

			if state == "sealed" {
				_, e = f.store.SealPrepared(t.Context(), req)
				require.NoError(t, e)
			} else {
				req.Seal.RevertOf = "old-op"
			}

			_, e = f.store.CutoverPrepared(t.Context(), req)
			require.Error(t, e)
			require.Equal(t, model.ChannelStatusDisabled, migrationStatus(t, f.store.DB, 3))
		})
	}
}

func TestAtomicCutoverRevisionBounds(t *testing.T) {
	for _, rev := range []int64{math.MaxInt64 - 2, math.MaxInt64 - 1, math.MaxInt64} {
		t.Run(strconv.FormatInt(rev, 10), func(t *testing.T) {
			f := newAdmissionFixture(t)
			req, _ := migrationFixture(t, f, 3)
			db := f.store.DB
			_, e := f.store.PrepareMigration(t.Context(), req)
			require.NoError(t, e)
			require.NoError(
				t,
				db.Model(&model.ChannelAdmissionControl{}).
					Where("id=1").
					UpdateColumn("revision", rev).
					Error,
			)
			before := migrationConfig(t, db)

			got, e := f.store.CutoverPrepared(t.Context(), req)
			if rev > math.MaxInt64-2 {
				require.Error(t, e)
				cutoverUnchanged(t, f, req, before, rev)
				return
			}

			require.NoError(t, e)
			require.EqualValues(t, math.MaxInt64, got.CommitRevision)
			replay, e := f.store.CutoverPrepared(t.Context(), req)
			require.NoError(t, e)
			require.Equal(t, got, replay)
			read, e := f.store.ReadPreparedMigration(t.Context(), req.Seal.OperationID)
			require.NoError(t, e)
			require.Equal(t, got, read)
			require.Equal(
				t,
				"ready",
				f.store.ReadPreparedMigrationFacts(t.Context(), req.Seal.OperationID).State,
			)
			_, e = f.store.Seal(t.Context(), f.seal("exhausted", math.MaxInt64, 3, 5, ""))
			require.Error(t, e)
		})
	}
}

func TestAtomicCutoverRejectsShadowBindingChange(t *testing.T) {
	f := newAdmissionFixture(t)
	_, _, req := lifecycleFixture(t, f)
	_, err := f.store.PrepareMigration(t.Context(), req)
	require.NoError(t, err)
	before := migrationConfig(t, f.store.DB)
	require.NoError(
		t,
		f.store.DB.Model(&model.ChannelMigrationShadowResource{}).
			Where("operation_id=?", req.Seal.OperationID).
			Update("prepared_request_hash", "changed").
			Error,
	)
	_, err = f.store.CutoverPrepared(t.Context(), req)
	require.Error(t, err)
	require.Equal(t, before, migrationConfig(t, f.store.DB))
	require.Equal(
		t,
		model.ChannelStatusDisabled,
		migrationStatus(t, f.store.DB, req.Seal.To.ChannelID),
	)
}
