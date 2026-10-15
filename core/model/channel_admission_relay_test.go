package model_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestAdmissionRuntimeRelayGenerationsCannotReplayAcrossSeal(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	channel, err := model.GetChannelByID(1)
	require.NoError(t, err)
	scope := model.RelayAdmissionScope{GroupID: "group", TokenID: 1, BillingOperationID: "billing", RequestID: "customer-reused-id", Model: "synthetic"}
	first, err := model.AdmitRelayAttempt(context.Background(), channel, scope)
	require.NoError(t, err)
	second, err := model.AdmitRelayAttempt(context.Background(), channel, scope)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)
	require.Equal(t, first.ResourceKey, second.ResourceKey)
	require.Equal(t, first.RequestHash, second.RequestHash)
	_, err = f.store.Seal(context.Background(), f.seal("seal", 0, 1, 2, ""))
	require.NoError(t, err)
	_, err = model.AdmitRelayAttempt(context.Background(), channel, scope)
	require.ErrorIs(t, err, model.ErrChannelAdmissionSealed)
	require.Len(t, runtimeUses(t, f.store.DB), 2)
	for _, use := range runtimeUses(t, f.store.DB) {
		require.Equal(t, "open", use.State)
	}
	successor, err := model.GetChannelByID(2)
	require.NoError(t, err)
	next, err := model.AdmitRelayAttempt(context.Background(), successor, scope)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, next.ID)
	require.Equal(t, first.KeyDigest, next.KeyDigest)
	require.NotEqual(t, first.IdentityHash, next.IdentityHash)
}

func TestAdmissionRuntimeRelayInternalScopeAndStaleIdentity(t *testing.T) {
	for _, scenario := range []string{"internal", "empty-billing", "stale-key", "stale-host", "preview", "disabled", "revoked", "invalid-token"} {
		t.Run(scenario, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			channel, err := model.GetChannelByID(1)
			require.NoError(t, err)
			scope := model.RelayAdmissionScope{BillingOperationID: "internal-server-id", RequestID: "internal"}
			switch scenario {
			case "empty-billing":
				scope.BillingOperationID = ""
			case "stale-key":
				channel.Key = "stale-synthetic"
			case "stale-host":
				channel.BaseURL = "https://stale.invalid"
			case "preview":
				channel.ID = 0
			case "invalid-token":
				scope.TokenID = -1
			case "disabled":
				require.NoError(t, f.store.DB.Model(&model.Channel{}).Where("id = ?", 1).Update("status", model.ChannelStatusDisabled).Error)
			case "revoked":
				require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionProof{}).Where("id = ?", f.proof.ProofID).Update("revoked", true).Error)
			}
			use, err := model.AdmitRelayAttempt(context.Background(), channel, scope)
			if scenario == "internal" {
				require.NoError(t, err)
				require.Equal(t, "open", use.State)
			} else {
				require.Error(t, err)
				require.Nil(t, use)
				require.Empty(t, runtimeUses(t, f.store.DB))
			}
		})
	}
	runtimeAdmissionFixture(t)
	channel, err := model.GetChannelByID(1)
	require.NoError(t, err)
	_, err = model.AdmitRelayAttempt(context.Background(), channel, model.RelayAdmissionScope{GroupID: strings.Repeat("x", 129), BillingOperationID: "id"})
	require.Error(t, err)
}

func TestAdmissionRuntimeRelayNeverActivatedKeepsLegacyCalls(t *testing.T) {
	for _, scenario := range []string{"no-storage", "installed-inactive", "partial"} {
		t.Run(scenario, func(t *testing.T) {
			db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "legacy.db"))
			require.NoError(t, err)
			pool, err := db.DB()
			require.NoError(t, err)
			defer pool.Close()
			oldDB, oldLog := model.DB, model.LogDB
			model.DB, model.LogDB = db, db
			defer func() { model.DB, model.LogDB = oldDB, oldLog }()
			if scenario != "no-storage" {
				require.NoError(t, db.AutoMigrate(&model.ChannelAdmissionControl{}))
				require.NoError(t, db.Create(&model.ChannelAdmissionControl{ID: 1, Version: 1}).Error)
				if scenario == "installed-inactive" {
					require.NoError(t, db.AutoMigrate(&model.ChannelAdmissionProof{}, &model.ChannelAdmissionFamily{}, &model.ChannelAdmissionBarrier{}, &model.ChannelAdmissionUse{}))
				}
			}
			// Temporary admin previews with no durable ID remain compatible only
			// before activation. Missing storage must not manufacture a proof/use.
			use, err := model.AdmitRelayAttempt(context.Background(), &model.Channel{Key: "synthetic-preview"}, model.RelayAdmissionScope{})
			require.Nil(t, use)
			if scenario == "partial" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
