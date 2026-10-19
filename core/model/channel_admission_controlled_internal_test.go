package model

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A store may already have been loaded when a copied empty control prefix is
// observed under its transaction lock. Controlled mode must not take the same
// legacy shortcut that remains supported by ordinary never-activated services.
func TestControlledPrefixCannotEnterLegacyShortcut(t *testing.T) {
	old := admissionRuntime.Load()
	defer admissionRuntime.Store(old)

	admissionRuntime.Store(&channelAdmissionRuntime{controlled: true})

	legacy, err := channelAdmissionNeverActivated(nil, &ChannelAdmissionControl{ID: 1, Version: 1})
	require.ErrorIs(t, err, ErrChannelAdmissionUnavailable)
	require.False(t, legacy)
}

func TestControlledValidMigrationAuthorizationRemainsDisabled(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "empty.sqlite"))
	require.NoError(t, err)

	defer func() { p, _ := db.DB(); p.Close() }()

	digest := strings.Repeat("a", 64)
	input := ChannelMigrationRequest{
		Seal: ChannelSealRequest{
			OperationID: "controlled-op",
			Epoch:       "epoch",
			From:        ChannelIdentity{ChannelID: 1, KeyDigest: digest, IdentityHash: digest},
			To:          ChannelIdentity{ChannelID: 2, KeyDigest: digest, IdentityHash: digest},
		},
		FromCredentialID: "same-key",
		ToCredentialID:   "same-key",
		ExpectedPinHash:  digest,
		ProjectionHash:   digest,
		FamilyHash:       digest,
	}
	loads := 0
	store := &ChannelAdmissionStore{
		DB:    db,
		LogDB: db,
		LoadDeployment: func(context.Context) (VerifiedDeploymentProof, error) {
			loads++
			return VerifiedDeploymentProof{StorageGeneration: "local"}, nil
		},
		LoadMigrationProbe: func(_ context.Context, r ChannelMigrationRequest) (VerifiedMigrationProbe, error) {
			loads++
			h, _ := admissionHash(r)

			return VerifiedMigrationProbe{
				RequestHash:       h,
				EvidenceHash:      digest,
				StorageGeneration: "local",
				IssuedAtMs:        1,
				ValidUntilMs:      2,
			}, nil
		},
	}
	// The ordinary command authorization accepts the full request and both
	// loaders. Controlled mode must reject before invoking either source.
	_, err = store.migrationAuthorization(t.Context(), input)
	require.NoError(t, err)
	require.Equal(t, 2, loads)

	store.controlledRuntime = true
	_, err = store.PrepareMigration(t.Context(), input)
	require.ErrorIs(t, err, ErrChannelAdmissionUnavailable)
	_, err = store.SealPrepared(t.Context(), input)
	require.ErrorIs(t, err, ErrChannelAdmissionUnavailable)
	_, err = store.CommitPreparedPins(t.Context(), input, 1)
	require.ErrorIs(t, err, ErrChannelAdmissionUnavailable)
	_, err = store.CutoverPrepared(t.Context(), input)
	require.ErrorIs(t, err, ErrChannelAdmissionUnavailable)
	_, err = store.Seal(t.Context(), input.Seal)
	require.ErrorIs(t, err, ErrChannelAdmissionUnavailable)

	input.TargetResourceKind = MigrationReuseRetiredTarget
	input.Seal.RevertOf = "prior-op"
	_, err = store.PrepareReverseMigration(t.Context(), input)
	require.ErrorIs(t, err, ErrChannelAdmissionUnavailable)
	_, err = store.CutoverReversePrepared(t.Context(), input)
	require.ErrorIs(t, err, ErrChannelAdmissionUnavailable)
	require.Equal(t, 2, loads)
}
