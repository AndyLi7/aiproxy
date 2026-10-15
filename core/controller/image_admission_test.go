package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/imageprepayment"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func activateImageAdmission(t *testing.T, h *imageCallLogHarness) *model.ChannelAdmissionStore {
	t.Helper()
	db := h.db
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.ChannelAdmissionControl{}, &model.ChannelAdmissionProof{}, &model.ChannelAdmissionFamily{}, &model.ChannelAdmissionBarrier{}, &model.ChannelAdmissionUse{}))
	h.channel.Status = model.ChannelStatusEnabled
	require.NoError(t, db.Create(h.channel).Error)
	executor := model.ChannelExecutorIdentity{ServiceID: "synthetic-image", InstanceID: "synthetic", BuildSHA: strings.Repeat("a", 64), Epoch: "synthetic-epoch"}
	proof := model.VerifiedDeploymentProof{ProofID: "synthetic-proof", Epoch: executor.Epoch, Sequence: 1, StorageGeneration: "synthetic-storage", ExecutorFenceHash: strings.Repeat("b", 64), IssuedAtMs: time.Now().Add(-time.Minute).UnixMilli(), ValidUntilMs: time.Now().Add(time.Minute).UnixMilli(), RevocationFreshUntilMs: time.Now().Add(time.Minute).UnixMilli(), Executors: []model.ChannelExecutorIdentity{executor}}
	raw, err := json.Marshal(proof.Executors)
	require.NoError(t, err)
	hash := sha256.Sum256(raw)
	proof.MembershipHash = hex.EncodeToString(hash[:])
	fact, err := proof.Fingerprint()
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.ChannelAdmissionControl{ID: 1, Version: 1, Epoch: proof.Epoch, EpochSequence: 1, StorageGeneration: proof.StorageGeneration, MembershipHash: proof.MembershipHash, ExecutorFenceHash: proof.ExecutorFenceHash, ActiveProofID: proof.ProofID}).Error)
	require.NoError(t, db.Create(&model.ChannelAdmissionProof{ID: proof.ProofID, FactHash: fact, ConsumedSequence: 1}).Error)
	store := &model.ChannelAdmissionStore{DB: db, LogDB: db, Executor: executor, LoadDeployment: func(context.Context) (model.VerifiedDeploymentProof, error) { return proof, nil }}
	oldDB := model.DB
	model.DB = db
	restore := model.ConfigureChannelAdmissionRuntime(executor, store.LoadDeployment)
	t.Cleanup(func() { restore(); model.DB = oldDB })
	return store
}
func imageAdmissionUses(t *testing.T, h *imageCallLogHarness) []model.ChannelAdmissionUse {
	t.Helper()
	var uses []model.ChannelAdmissionUse
	require.NoError(t, h.db.Order("admitted_at_ms,id").Find(&uses).Error)
	return uses
}
func replaceImageAdmissionGeneration(t *testing.T, h *imageCallLogHarness) *model.ImageTask {
	t.Helper()
	var current model.ImageTask
	require.NoError(t, h.db.First(&current, "id = ?", h.requestID).Error)
	require.NoError(t, model.CaptureImageTaskGeneration(&current))
	require.NoError(t, model.ReleaseImageTaskReservationGeneration(&current))
	identity, err := model.ChannelIdentityOf(h.channel)
	require.NoError(t, err)
	next := current
	next.AdmissionGeneration = ""
	next.AdmissionAttempt = ""
	next.AdmissionIdentityHash = identity.IdentityHash
	next.BillingOperationID = "replacement-billing"
	next.UsageID = 0
	saved, created, err := model.ReserveImageTask(&next, &model.AsyncUsageInfo{RequestID: next.ID, ChannelID: h.channel.ID, BaseURL: h.channel.BaseURL, GroupID: next.GroupID, TokenID: next.TokenID})
	require.NoError(t, err)
	require.True(t, created)
	return saved
}
func TestImageAdmissionActualPrepaidEntryAndFrozenFirstSend(t *testing.T) {
	h := newImageCallLogHarness(t, http.StatusOK)
	store := activateImageAdmission(t, h)
	next := *h.channel
	next.ID = 8
	require.NoError(t, h.db.Create(&next).Error)
	h.walletHook = func(command balance.PrepaymentCommand) {
		if command.Action == "admit" {
			require.Len(t, imageAdmissionUses(t, h), 1)
			from, err := model.ChannelIdentityOf(h.channel)
			require.NoError(t, err)
			to, err := model.ChannelIdentityOf(&next)
			require.NoError(t, err)
			_, err = store.Seal(context.Background(), model.ChannelSealRequest{OperationID: "move", Epoch: "synthetic-epoch", From: from, To: to})
			require.NoError(t, err)
		}
		if command.Action == "begin_attempt" {
			require.Len(t, imageAdmissionUses(t, h), 2)
		}
	}
	h.providerHook = func() { require.Len(t, imageAdmissionUses(t, h), 2) }
	result := h.submit()
	require.Equal(t, http.StatusAccepted, result.Code, result.Body.String())
	require.Equal(t, 1, h.providerCalls)
	for _, use := range imageAdmissionUses(t, h) {
		require.Equal(t, "open", use.State)
	}
	// A distinct task after the seal is refused before wallet admission.
	h.requestID = "after-seal"
	h.walletHook = nil
	count := len(h.commands)
	result = h.submit()
	require.Equal(t, http.StatusServiceUnavailable, result.Code)
	require.Len(t, h.commands, count)
	require.Equal(t, 1, h.providerCalls)
}
func TestImageAdmissionActualRefusalReleasesOnlyItsGeneration(t *testing.T) {
	for _, scenario := range []string{"refusal", "late-refusal", "late-refusal-activation", "late-positive", "wallet-unknown"} {
		t.Run(scenario, func(t *testing.T) {
			code := http.StatusPaymentRequired
			if scenario == "late-positive" {
				code = http.StatusOK
			}
			if scenario == "wallet-unknown" {
				code = http.StatusBadGateway
			}
			h := newImageCallLogHarness(t, code)
			if scenario != "late-refusal-activation" {
				activateImageAdmission(t, h)
			}
			var replacement *model.ImageTask
			if scenario == "late-refusal" || scenario == "late-positive" || scenario == "late-refusal-activation" {
				h.walletHook = func(command balance.PrepaymentCommand) {
					if command.Action == "admit" {
						if scenario == "late-refusal-activation" {
							activateImageAdmission(t, h)
						}
						replacement = replaceImageAdmissionGeneration(t, h)
					}
				}
			}
			result := h.submit()
			require.Zero(t, h.providerCalls)
			if scenario == "refusal" {
				require.Equal(t, http.StatusPaymentRequired, result.Code)
				uses := imageAdmissionUses(t, h)
				require.Len(t, uses, 1)
				require.Equal(t, "closed", uses[0].State)
				var count int64
				require.NoError(t, h.db.Model(&model.ImageTask{}).Count(&count).Error)
				require.Zero(t, count)
			} else {
				require.Equal(t, http.StatusServiceUnavailable, result.Code)
				var task model.ImageTask
				require.NoError(t, h.db.First(&task, "id = ?", h.requestID).Error)
				require.Equal(t, "submitting", task.Status)
				require.Nil(t, task.Billing)
				require.Empty(t, task.Attempts)
				if replacement != nil {
					require.Equal(t, replacement.BillingOperationID, task.BillingOperationID)
				}
			}
		})
	}
}
func TestImageAdmissionActualRecoveryWalletReplyCannotWriteReplacement(t *testing.T) {
	h := newImageCallLogHarness(t, http.StatusBadGateway)
	activateImageAdmission(t, h)
	result := h.submit()
	require.Equal(t, http.StatusServiceUnavailable, result.Code)
	var original model.ImageTask
	require.NoError(t, h.db.First(&original, "id = ?", h.requestID).Error)
	var replacement *model.ImageTask
	h.walletHook = func(command balance.PrepaymentCommand) {
		if command.Action == "get" {
			replacement = replaceImageAdmissionGeneration(t, h)
		}
	}
	_, err := imageprepayment.Sync(t.Context(), &original)
	require.ErrorIs(t, err, model.ErrChannelAdmissionConflict)
	require.NotNil(t, replacement)
	var current model.ImageTask
	require.NoError(t, h.db.First(&current, "id = ?", h.requestID).Error)
	require.Nil(t, current.Billing)
	require.Equal(t, replacement.BillingOperationID, current.BillingOperationID)
	require.Equal(t, "submitting", current.Status)
	require.Zero(t, h.providerCalls)
}
