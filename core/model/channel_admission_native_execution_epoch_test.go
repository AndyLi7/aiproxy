package model_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func TestIndependentNativeExecutionAfterLegitimateEpochActivation(t *testing.T) {
	for _, prior := range []bool{false, true} {
		name := "no-prior-record"
		if prior {
			name = "prior-record"
		}
		t.Run(name, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installNativeExecution(t, db)
			task := executionNativeTask(t, f)
			if prior {
				h, err := model.BeginNativeTaskExecution(t.Context(), db, task, model.NativeExecutionPoll, "")
				require.NoError(t, err)
				require.NotNil(t, h)
				require.NoError(t, h.RecordReturn(t.Context(), false))
			}
			f.proof.ProofID = "synthetic-proof-epoch-2"
			f.proof.Sequence = 2
			f.proof.Epoch = "synthetic-epoch-2"
			f.store.Executor.Epoch = f.proof.Epoch
			f.store.Executor.InstanceID = "next-instance"
			f.proof.Executors = []model.ChannelExecutorIdentity{f.store.Executor}
			raw, err := json.Marshal(f.proof.Executors)
			require.NoError(t, err)
			sum := sha256.Sum256(raw)
			f.proof.MembershipHash = hex.EncodeToString(sum[:])
			activated, err := f.store.Activate(t.Context(), 1)
			require.NoError(t, err)
			require.Equal(t, int64(2), activated.EpochSequence)
			restore := model.ConfigureChannelAdmissionRuntime(f.store.Executor, f.store.LoadDeployment)
			defer restore()
			newTask, created, err := model.ReserveNativeTask(db, runtimeNativeTask(f, "after-epoch", 0))
			require.NoError(t, err)
			require.True(t, created)
			require.NotNil(t, newTask)
			h, err := model.BeginNativeTaskExecution(t.Context(), db, task, model.NativeExecutionPoll, "")
			t.Logf("new epoch activation and new task succeeded; old task prior_record=%v begin_error=%v", prior, err)
			require.NoError(t, err, "a valid new executor must be able to continue the frozen old task")
			require.NotNil(t, h)
			require.NoError(t, h.RecordReturn(t.Context(), false))
		})
	}
}

func advanceNativeExecutionEpoch(t *testing.T, f *admissionFixture, sequence int64, sameEpoch bool) func() {
	t.Helper()
	f.proof.ProofID = fmt.Sprintf("epoch-proof-%d", sequence)
	f.proof.Sequence = sequence
	if !sameEpoch {
		f.proof.Epoch = fmt.Sprintf("epoch-%d", sequence)
	}
	f.proof.IssuedAtMs = time.Now().Add(-time.Second).UnixMilli()
	f.proof.ValidUntilMs = time.Now().Add(time.Minute).UnixMilli()
	f.proof.RevocationFreshUntilMs = f.proof.ValidUntilMs
	f.store.Executor.Epoch = f.proof.Epoch
	f.store.Executor.InstanceID = fmt.Sprintf("instance-%d", sequence)
	f.proof.Executors = []model.ChannelExecutorIdentity{f.store.Executor}
	raw, err := json.Marshal(f.proof.Executors)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	f.proof.MembershipHash = hex.EncodeToString(sum[:])
	_, err = f.store.Activate(t.Context(), sequence-1)
	require.NoError(t, err)
	return model.ConfigureChannelAdmissionRuntime(f.store.Executor, f.store.LoadDeployment)
}

func assertNativeContinuousEpochHistory(t *testing.T, f *admissionFixture) {
	db := f.store.DB
	installNativeExecution(t, db)
	task := executionNativeTask(t, f)
	deadline := time.Now().Add(1500 * time.Millisecond)
	f.proof.ValidUntilMs = deadline.UnixMilli()
	f.proof.RevocationFreshUntilMs = deadline.UnixMilli()
	hash, err := f.proof.Fingerprint()
	require.NoError(t, err)
	require.NoError(t, db.Model(&model.ChannelAdmissionProof{}).Where("id = ?", f.proof.ProofID).Update("fact_hash", hash).Error)
	active, err := model.BeginNativeTaskExecution(t.Context(), db, task, model.NativeExecutionPoll, "")
	require.NoError(t, err)
	returned, err := model.BeginNativeTaskExecution(t.Context(), db, task, model.NativeExecutionBilling, "")
	require.NoError(t, err)
	require.NoError(t, returned.RecordReturn(t.Context(), false))
	original := nativeExecutionRows(t, db)
	require.Len(t, original, 2)
	if delay := time.Until(deadline.Add(30 * time.Millisecond)); delay > 0 {
		time.Sleep(delay)
	}
	for sequence := int64(2); sequence <= 3; sequence++ {
		restore := advanceNativeExecutionEpoch(t, f, sequence, false)
		defer restore()
		h, err := model.BeginNativeTaskExecution(t.Context(), db, task, model.NativeExecutionPoll, "")
		require.NoError(t, err)
		require.NotNil(t, h)
		require.NoError(t, h.RecordReturn(t.Context(), false))
		rows := nativeExecutionRows(t, db)
		require.Len(t, rows, int(sequence)+1)
		for _, old := range original {
			var current model.ChannelAdmissionTaskExecution
			require.NoError(t, db.First(&current, "id = ?", old.ID).Error)
			require.Equal(t, old, current, "epoch activation must not rewrite or finish historical rows")
		}
		for _, row := range rows {
			var history model.VerifiedDeploymentProof
			require.NoError(t, json.Unmarshal([]byte(row.ExecutorProofJSON), &history))
			require.Equal(t, row.Epoch, history.Epoch)
			require.Equal(t, "unproven", row.ContinuationState)
		}
	}
	// Only the original private capability may report this old active call's return.
	require.NoError(t, active.RecordReturn(t.Context(), false))
	require.Equal(t, "open", runtimeUses(t, db)[0].State)
}
func TestAdmissionNativeExecutionContinuousEpochHistory(t *testing.T) {
	assertNativeContinuousEpochHistory(t, runtimeAdmissionFixture(t))
}
func TestPostgresNativeExecutionContinuousEpochHistory(t *testing.T) {
	assertNativeContinuousEpochHistory(t, runtimeAdmissionOnFixture(t, admissionFixtureOn(t, queueLockPostgres(t), "")))
}

func TestAdmissionNativeExecutionEpochRejectsUntrustedHistoryAndCurrentProof(t *testing.T) {
	for _, change := range []string{"valid", "same-epoch-member", "missing-new-proof", "unavailable-new-proof", "wrong-new-epoch", "wrong-new-member", "wrong-new-fingerprint", "revoked-new-proof", "expired-new-proof", "storage", "history-missing", "history-json", "history-epoch", "history-member", "history-storage", "history-sequence", "history-proof-id", "history-proof-hash", "history-revoked", "history-record-epoch", "history-record-member", "legacy-row", "expired-owner", "changed-owner"} {
		t.Run(change, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installNativeExecution(t, db)
			task := executionNativeTask(t, f)
			h, err := model.BeginNativeTaskExecution(t.Context(), db, task, model.NativeExecutionPoll, "")
			require.NoError(t, err)
			require.NoError(t, h.RecordReturn(t.Context(), false))
			original := nativeExecutionRows(t, db)[0]
			var oldProof model.VerifiedDeploymentProof
			require.NoError(t, json.Unmarshal([]byte(original.ExecutorProofJSON), &oldProof))
			restore := advanceNativeExecutionEpoch(t, f, 2, change == "same-epoch-member")
			defer restore()
			currentProof := f.proof
			executor := f.store.Executor
			load := f.store.LoadDeployment
			switch change {
			case "missing-new-proof":
				load = nil
			case "unavailable-new-proof":
				load = func(context.Context) (model.VerifiedDeploymentProof, error) {
					return model.VerifiedDeploymentProof{}, errors.New("unavailable")
				}
			case "wrong-new-epoch":
				currentProof.Epoch = "wrong"
				load = func(context.Context) (model.VerifiedDeploymentProof, error) { return currentProof, nil }
			case "wrong-new-member":
				executor.InstanceID = "not-a-member"
			case "wrong-new-fingerprint":
				currentProof.ValidUntilMs++
				load = func(context.Context) (model.VerifiedDeploymentProof, error) { return currentProof, nil }
			case "revoked-new-proof":
				require.NoError(t, db.Model(&model.ChannelAdmissionProof{}).Where("id = ?", currentProof.ProofID).Update("revoked", true).Error)
			case "expired-new-proof":
				currentProof.ValidUntilMs = time.Now().Add(-time.Millisecond).UnixMilli()
				hash, err := currentProof.Fingerprint()
				require.NoError(t, err)
				require.NoError(t, db.Model(&model.ChannelAdmissionProof{}).Where("id = ?", currentProof.ProofID).Update("fact_hash", hash).Error)
				load = func(context.Context) (model.VerifiedDeploymentProof, error) { return currentProof, nil }
			case "storage":
				require.NoError(t, db.Model(&model.ChannelAdmissionControl{}).Where("id = 1").Update("storage_generation", "restored").Error)
			case "history-missing":
				require.NoError(t, db.Model(&original).Update("executor_proof_json", "").Error)
			case "history-json":
				require.NoError(t, db.Model(&original).Update("executor_proof_json", "{").Error)
			case "history-epoch":
				oldProof.Epoch = "invented"
			case "history-member":
				oldProof.Executors[0].InstanceID = "invented"
			case "history-storage":
				oldProof.StorageGeneration = "invented"
			case "history-sequence":
				oldProof.Sequence = 2
			case "history-proof-id":
				oldProof.ProofID = "missing"
			case "history-proof-hash":
				require.NoError(t, db.Model(&model.ChannelAdmissionProof{}).Where("id = ?", oldProof.ProofID).Update("fact_hash", strings.Repeat("a", 64)).Error)
			case "history-revoked":
				require.NoError(t, db.Model(&model.ChannelAdmissionProof{}).Where("id = ?", oldProof.ProofID).Update("revoked", true).Error)
			case "history-record-epoch":
				require.NoError(t, db.Model(&original).Update("epoch", "invented").Error)
			case "history-record-member":
				require.NoError(t, db.Model(&original).Update("instance_id", "invented").Error)
			case "legacy-row":
				require.NoError(t, db.Model(&original).Updates(map[string]any{"version": 1, "executor_proof_json": ""}).Error)
			}
			switch change {
			case "history-epoch", "history-member", "history-storage", "history-sequence", "history-proof-id":
				raw, err := json.Marshal(oldProof)
				require.NoError(t, err)
				require.NoError(t, db.Model(&original).Update("executor_proof_json", string(raw)).Error)
			}
			restoreSource := model.ConfigureChannelAdmissionRuntime(executor, load)
			defer restoreSource()
			owner := ""
			until := int64(0)
			if change == "expired-owner" || change == "changed-owner" {
				owner = "old-owner"
				until = time.Now().Add(time.Minute).Unix()
				dbOwner := owner
				deadline := until
				if change == "expired-owner" {
					deadline = time.Now().Add(-time.Second).Unix()
				} else {
					dbOwner = "new-owner"
				}
				require.NoError(t, db.Model(&model.NativeTask{}).Where("id = ?", task.ID).Updates(map[string]any{"recovery_owner": dbOwner, "recovery_until": deadline}).Error)
			}
			next, err := model.BeginNativeTaskExecution(t.Context(), db, task, model.NativeExecutionPoll, owner, until)
			if change == "valid" || change == "same-epoch-member" {
				require.NoError(t, err)
				require.NotNil(t, next)
				require.NoError(t, next.RecordReturn(t.Context(), false))
				require.Len(t, nativeExecutionRows(t, db), 2)
			} else {
				require.Error(t, err)
				require.Nil(t, next)
				require.Len(t, nativeExecutionRows(t, db), 1)
			}
			require.Equal(t, "open", runtimeUses(t, db)[0].State)
		})
	}
}

func assertUnpublishedNativeV1RequiresReview(t *testing.T, f *admissionFixture) {
	db := f.store.DB
	task := executionNativeTask(t, f)
	installNativeExecutionDraft(t, db, "v1")
	// The fresh-v2 draft deliberately cannot upgrade an existing v1 journal.
	installNativeExecutionDraft(t, db, "v2")
	h, err := model.BeginNativeTaskExecution(t.Context(), db, task, model.NativeExecutionPoll, "")
	require.Error(t, err)
	require.Nil(t, h)
	var protocol model.ChannelAdmissionExecutionProtocol
	require.NoError(t, db.First(&protocol, "id = ?", 2).Error)
	require.Equal(t, 1, protocol.Version)
	require.False(t, db.Migrator().HasColumn(&model.ChannelAdmissionTaskExecution{}, "executor_proof_json"))
	require.Equal(t, "open", runtimeUses(t, db)[0].State)
}
func TestAdmissionNativeExecutionUnpublishedV1RequiresReview(t *testing.T) {
	assertUnpublishedNativeV1RequiresReview(t, runtimeAdmissionFixture(t))
}
func TestPostgresNativeExecutionUnpublishedV1RequiresReview(t *testing.T) {
	assertUnpublishedNativeV1RequiresReview(t, runtimeAdmissionOnFixture(t, admissionFixtureOn(t, queueLockPostgres(t), "")))
}

func TestAdmissionNativeExecutionNoHistoryCannotBorrowAcrossEpochOutage(t *testing.T) {
	for _, extension := range []bool{false, true} {
		t.Run(fmt.Sprint(extension), func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			task := executionNativeTask(t, f)
			if extension {
				installNativeExecution(t, db)
			}
			restore := advanceNativeExecutionEpoch(t, f, 2, false)
			defer restore()
			restoreMissing := model.ConfigureChannelAdmissionRuntime(f.store.Executor, nil)
			defer restoreMissing()
			h, err := model.BeginNativeTaskExecution(t.Context(), db, task, model.NativeExecutionPoll, "")
			if extension {
				require.Error(t, err)
				require.Empty(t, nativeExecutionRows(t, db))
			} else {
				require.NoError(t, err)
			}
			require.Nil(t, h)
			require.Equal(t, "open", runtimeUses(t, db)[0].State)
		})
	}
}
