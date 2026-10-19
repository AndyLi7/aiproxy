package model_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestControlledRevokeExactProofIdempotent(t *testing.T) {
	f := newAdmissionFixture(t)
	hash, e := f.proof.Fingerprint()
	require.NoError(t, e)
	require.Error(
		t,
		model.RevokeDeploymentProof(
			t.Context(),
			f.store.DB,
			f.proof.ProofID,
			strings.Repeat("0", 64),
		),
	)
	require.NoError(t, model.RevokeDeploymentProof(t.Context(), f.store.DB, f.proof.ProofID, hash))
	require.NoError(t, model.RevokeDeploymentProof(t.Context(), f.store.DB, f.proof.ProofID, hash))
	_, _, e = f.store.Admit(t.Context(), admissionRequest("late", f.identities[0]), nil)
	require.Error(t, e)
}

func TestPostgresControlledRevokeWaitAndFinalAuthorization(t *testing.T) {
	for _, mode := range []string{"channel-wait", "proof-row-wait", "wrong-hash"} {
		t.Run(mode, func(t *testing.T) {
			db := queueLockPostgres(t)
			f := admissionFixtureOn(t, db, "")
			hash, e := f.proof.Fingerprint()
			require.NoError(t, e)
			pool, e := db.DB()
			require.NoError(t, e)

			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()

			holder, e := pool.BeginTx(ctx, nil)
			require.NoError(t, e)

			defer func() {
				if err := holder.
					Rollback(); err !=
					nil &&
					!errors.Is(
						err, sql.
							ErrTxDone,
					) {
					t.Error(err)
				}
			}()

			if mode == "channel-wait" {
				var id int
				require.NoError(
					t,
					holder.QueryRowContext(ctx, "SELECT id FROM channels WHERE id=1 FOR UPDATE").
						Scan(&id),
				)

				done := make(chan error, 1)
				go func() { _, _, e := f.store.Admit(ctx, admissionRequest("delayed", f.identities[0]), nil); done <- e }()

				require.Eventually(t, func() bool {
					var n int64

					return db.Raw("SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'").
						Scan(&n).
						Error == nil &&
						n > 0
				}, time.Second, 5*time.Millisecond)
				require.NoError(t, model.RevokeDeploymentProof(ctx, db, f.proof.ProofID, hash))
				require.NoError(t, holder.Commit())
				require.Error(t, <-done)

				var n int64
				require.NoError(t, db.Model(&model.ChannelAdmissionUse{}).Count(&n).Error)
				require.Zero(t, n)
			} else {
				var id string
				require.NoError(
					t,
					holder.QueryRowContext(ctx, "SELECT id FROM channel_admission_proofs WHERE id=$1 FOR UPDATE", f.proof.ProofID).
						Scan(&id),
				)

				expected := hash
				if mode == "wrong-hash" {
					expected = strings.Repeat("0", 64)
				}

				done := make(chan error, 1)
				go func() { done <- model.RevokeDeploymentProof(ctx, db, f.proof.ProofID, expected) }()

				select {
				case earlyErr := <-done:
					t.Logf("unexpected early result: %v", earlyErr)
					t.Fatal("revoke bypassed row lock")
				case <-time.After(50 * time.Millisecond):
				}

				require.NoError(t, holder.Commit())

				e = <-done

				var p model.ChannelAdmissionProof
				require.NoError(t, db.First(&p, "id=?", id).Error)

				if mode == "wrong-hash" {
					require.Error(t, e)
					require.False(t, p.Revoked)
				} else {
					require.NoError(t, e)
					require.True(t, p.Revoked)
				}
			}
		})
	}
}
