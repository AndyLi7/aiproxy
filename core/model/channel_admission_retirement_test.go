package model_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func admissionPersistentState(t *testing.T, f *admissionFixture) string {
	t.Helper()
	var control []model.ChannelAdmissionControl
	var families []model.ChannelAdmissionFamily
	var barriers []model.ChannelAdmissionBarrier
	var uses []model.ChannelAdmissionUse
	var proofs []model.ChannelAdmissionProof
	require.NoError(t, f.store.DB.Order("id").Find(&control).Error)
	require.NoError(t, f.store.DB.Order("key_digest").Find(&families).Error)
	require.NoError(t, f.store.DB.Order("operation_id").Find(&barriers).Error)
	require.NoError(t, f.store.DB.Order("id").Find(&uses).Error)
	require.NoError(t, f.store.DB.Order("id").Find(&proofs).Error)
	encoded, err := json.Marshal([]any{control, families, barriers, uses, proofs})
	require.NoError(t, err)
	return string(encoded)
}

func TestAdmissionRetiredTargetRequiresApplicableExplicitInverse(t *testing.T) {
	for _, scenario := range []string{"implicit-same-key", "missing-operation", "unrelated-operation", "skip-generation-implicit", "skip-generation-explicit", "implicit-cross-key", "stale-exact-inverse"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAdmissionFixture(t)
			ctx := context.Background()
			_, err := f.store.Seal(ctx, f.seal("first", 0, 1, 2, ""))
			require.NoError(t, err)
			candidate := f.seal("rejected", 1, 2, 1, "")
			switch scenario {
			case "missing-operation":
				candidate.RevertOf = "missing"
			case "unrelated-operation":
				_, err = f.store.Seal(ctx, f.seal("unrelated", 1, 3, 5, ""))
				require.NoError(t, err)
				candidate = f.seal("rejected", 2, 2, 1, "unrelated")
			case "skip-generation-implicit", "skip-generation-explicit":
				_, err = f.store.Seal(ctx, f.seal("second", 1, 2, 4, ""))
				require.NoError(t, err)
				candidate = f.seal("rejected", 2, 4, 1, "")
				if scenario == "skip-generation-explicit" {
					candidate.RevertOf = "first"
				}
			case "implicit-cross-key":
				_, err = f.store.Seal(ctx, f.seal("rotation", 1, 2, 3, ""))
				require.NoError(t, err)
				candidate = f.seal("rejected", 2, 3, 2, "")
			case "stale-exact-inverse":
				_, err = f.store.Seal(ctx, f.seal("return", 1, 2, 1, "first"))
				require.NoError(t, err)
				_, err = f.store.Seal(ctx, f.seal("forward-again", 2, 1, 2, "return"))
				require.NoError(t, err)
				candidate = f.seal("rejected", 3, 2, 1, "first")
			}
			before := admissionPersistentState(t, f)
			_, err = f.store.Seal(ctx, candidate)
			require.ErrorIs(t, err, model.ErrChannelAdmissionConflict)
			require.Equal(t, before, admissionPersistentState(t, f), "rejected seal must not change any persistent protocol state")
			_, created, err := f.store.Admit(ctx, admissionRequest("must-remain-retired", candidate.To), nil)
			require.ErrorIs(t, err, model.ErrChannelAdmissionSealed)
			require.False(t, created)
			require.Equal(t, before, admissionPersistentState(t, f), "rejection must not create a use")
		})
	}
}

func TestAdmissionExplicitInverseFollowsRetirementStackAndKeepsIdempotency(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := context.Background()
	first := f.seal("first", 0, 1, 2, "")
	firstResult, err := f.store.Seal(ctx, first)
	require.NoError(t, err)
	_, err = f.store.Seal(ctx, f.seal("second", 1, 2, 4, ""))
	require.NoError(t, err)
	_, err = f.store.Seal(ctx, f.seal("undo-second", 2, 4, 2, "second"))
	require.NoError(t, err)
	_, err = f.store.Seal(ctx, f.seal("undo-first", 3, 2, 1, "first"))
	require.NoError(t, err)
	_, created, err := f.store.Admit(ctx, admissionRequest("restored-first", f.identities[0]), nil)
	require.NoError(t, err)
	require.True(t, created)
	before := admissionPersistentState(t, f)
	historical, err := f.store.Seal(ctx, first)
	require.NoError(t, err)
	require.Equal(t, firstResult, historical)
	require.Equal(t, before, admissionPersistentState(t, f), "historical result replay never changes the current successor")
	_, _, err = f.store.Admit(ctx, admissionRequest("still-retired-second", f.identities[1]), nil)
	require.ErrorIs(t, err, model.ErrChannelAdmissionSealed)
}
