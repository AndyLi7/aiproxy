package model_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestAdmissionNativeExecutionFrozenAnchorRejectsConflictingFacts(t *testing.T) {
	for _, change := range []string{"none", "storage", "use", "task", "channel", "record-version", "record-kind", "record-state", "record-hash", "other-conflicting-row", "executor", "owner", "expired", "shortened", "missing-column"} {
		t.Run(change, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installNativeExecution(t, db)
			task := executionNativeTask(t, f)
			require.NoError(
				t,
				db.Model(&model.NativeTask{}).
					Where("id = ?", task.ID).
					Updates(map[string]any{"recovery_owner": "claim", "recovery_until": time.Now().Add(time.Minute).Unix()}).
					Error,
			)
			task, err := model.GetNativeTask(db, task.ID, task.GroupID, task.TokenID)
			require.NoError(t, err)

			originalUntil := task.RecoveryUntil
			h, err := model.BeginNativeTaskExecution(
				t.Context(),
				db,
				task,
				model.NativeExecutionPoll,
				"claim",
				originalUntil,
			)
			require.NoError(t, err)
			require.NoError(t, h.RecordReturn(t.Context(), false))
			row := nativeExecutionRows(t, db)[0]

			executor := f.store.Executor
			switch change {
			case "storage":
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionControl{}).
						Where("id = 1").
						Update("storage_generation", "restored").
						Error,
				)
			case "use":
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionUse{}).
						Where("id = ?", row.UseID).
						Update("admitted_at_ms", row.StartedAtMs+1).
						Error,
				)
			case "task":
				require.NoError(
					t,
					db.Model(&model.NativeTask{}).
						Where("id = ?", task.ID).
						Update("billing_operation_id", "different").
						Error,
				)
			case "channel":
				require.NoError(
					t,
					db.Model(&model.Channel{}).
						Where("id = 1").
						Update("base_url", "https://different.invalid").
						Error,
				)
			case "record-version":
				require.NoError(t, db.Model(&row).Update("version", 99).Error)
			case "record-kind":
				require.NoError(t, db.Model(&row).Update("kind", "not-an-invocation").Error)
			case "record-state":
				require.NoError(t, db.Model(&row).Update("state", "closed").Error)
			case "record-hash":
				require.NoError(
					t,
					db.Model(&row).Update("task_hash", strings.Repeat("a", 64)).Error,
				)
			case "other-conflicting-row":
				row.ID = strings.Repeat("c", 64)
				row.TaskHash = strings.Repeat("a", 64)
				require.NoError(t, db.Create(&row).Error)
			case "executor":
				executor.InstanceID = "different-instance"
			case "owner":
				require.NoError(
					t,
					db.Model(&model.NativeTask{}).
						Where("id = ?", task.ID).
						Update("recovery_owner", "new-claim").
						Error,
				)
			case "expired":
				require.NoError(
					t,
					db.Model(&model.NativeTask{}).
						Where("id = ?", task.ID).
						Update("recovery_until", time.Now().Add(-time.Second).Unix()).
						Error,
				)
			case "shortened":
				require.NoError(
					t,
					db.Model(&model.NativeTask{}).
						Where("id = ?", task.ID).
						Update("recovery_until", originalUntil-1).
						Error,
				)
			case "missing-column":
				require.NoError(
					t,
					db.Exec("ALTER TABLE channel_admission_task_executions DROP COLUMN kind").Error,
				)
			}

			restore := model.ConfigureChannelAdmissionRuntime(executor, nil)
			defer restore()

			current, err := model.GetNativeTask(db, task.ID, task.GroupID, task.TokenID)
			require.NoError(t, err)

			next, err := model.BeginNativeTaskExecution(
				t.Context(),
				db,
				current,
				model.NativeExecutionPoll,
				"claim",
				originalUntil,
			)
			if change == "none" {
				require.NoError(t, err)
				require.NotNil(t, next)
				require.NoError(t, next.RecordReturn(t.Context(), false))
			} else {
				require.Error(t, err)
				require.Nil(t, next)
			}

			require.Equal(t, "open", runtimeUses(t, db)[0].State)
		})
	}
}

func TestAdmissionNativeExecutionNoAnchorDistinguishesOutageFromInvalidProof(t *testing.T) {
	for _, mode := range []string{"outage", "source-conflict", "partial-error-snapshot", "revoked", "missing-local-proof", "proof-mismatch", "storage-mismatch", "expired-proof"} {
		t.Run(mode, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			task := executionNativeTask(t, f)
			installNativeExecution(t, db)

			proof := f.proof

			load := func(context.Context) (model.VerifiedDeploymentProof, error) {
				return model.VerifiedDeploymentProof{}, errors.New("temporarily unavailable")
			}
			switch mode {
			case "source-conflict":
				load = func(context.Context) (model.VerifiedDeploymentProof, error) {
					return model.VerifiedDeploymentProof{}, model.ErrChannelAdmissionConflict
				}
			case "partial-error-snapshot":
				load = func(context.Context) (model.VerifiedDeploymentProof, error) {
					return proof, errors.New("rejected snapshot")
				}
			case "revoked":
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionProof{}).
						Where("id = ?", proof.ProofID).
						Update("revoked", true).
						Error,
				)
			case "missing-local-proof":
				require.NoError(
					t,
					db.Where("id = ?", proof.ProofID).Delete(&model.ChannelAdmissionProof{}).Error,
				)
			case "proof-mismatch":
				proof.ExecutorFenceHash = strings.Repeat("a", 64)
				load = func(context.Context) (model.VerifiedDeploymentProof, error) { return proof, nil }
			case "storage-mismatch":
				proof.StorageGeneration = "other-storage"
				load = func(context.Context) (model.VerifiedDeploymentProof, error) { return proof, nil }
			case "expired-proof":
				proof.ValidUntilMs = time.Now().Add(-time.Second).UnixMilli()
				hash, err := proof.Fingerprint()
				require.NoError(t, err)
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionProof{}).
						Where("id = ?", proof.ProofID).
						Update("fact_hash", hash).
						Error,
				)

				load = func(context.Context) (model.VerifiedDeploymentProof, error) { return proof, nil }
			}

			restore := model.ConfigureChannelAdmissionRuntime(f.store.Executor, load)
			defer restore()

			h, err := model.BeginNativeTaskExecution(
				t.Context(),
				db,
				task,
				model.NativeExecutionPoll,
				"",
			)
			if mode == "outage" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}

			require.Nil(t, h)
			require.Empty(t, nativeExecutionRows(t, db))
			require.Equal(t, "open", runtimeUses(t, db)[0].State)
		})
	}
}

func TestAdmissionNativeExecutionNewExecutorRequiresValidSnapshot(t *testing.T) {
	for _, mode := range []string{"missing", "valid", "revoked", "expired"} {
		t.Run(mode, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			nextExecutor := f.store.Executor
			nextExecutor.InstanceID = "second-instance"
			f.proof.Executors = append(f.proof.Executors, nextExecutor)
			raw, err := json.Marshal(f.proof.Executors)
			require.NoError(t, err)

			sum := sha256.Sum256(raw)
			f.proof.MembershipHash = hex.EncodeToString(sum[:])
			hash, err := f.proof.Fingerprint()
			require.NoError(t, err)
			require.NoError(
				t,
				db.Model(&model.ChannelAdmissionControl{}).
					Where("id = 1").
					Update("membership_hash", f.proof.MembershipHash).
					Error,
			)
			require.NoError(
				t,
				db.Model(&model.ChannelAdmissionProof{}).
					Where("id = ?", f.proof.ProofID).
					Update("fact_hash", hash).
					Error,
			)
			installNativeExecution(t, db)
			task := executionNativeTask(t, f)
			h, err := model.BeginNativeTaskExecution(
				t.Context(),
				db,
				task,
				model.NativeExecutionPoll,
				"",
			)
			require.NoError(t, err)
			require.NoError(t, h.RecordReturn(t.Context(), false))

			load := f.store.LoadDeployment
			switch mode {
			case "missing":
				load = nil
			case "revoked":
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionProof{}).
						Where("id = ?", f.proof.ProofID).
						Update("revoked", true).
						Error,
				)
			case "expired":
				f.proof.ValidUntilMs = time.Now().Add(-time.Second).UnixMilli()
				hash, err = f.proof.Fingerprint()
				require.NoError(t, err)
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionProof{}).
						Where("id = ?", f.proof.ProofID).
						Update("fact_hash", hash).
						Error,
				)
			}

			restore := model.ConfigureChannelAdmissionRuntime(nextExecutor, load)
			defer restore()

			second, err := model.BeginNativeTaskExecution(
				t.Context(),
				db,
				task,
				model.NativeExecutionPoll,
				"",
			)
			if mode == "valid" {
				require.NoError(t, err)
				require.NotNil(t, second)
				require.NoError(t, second.RecordReturn(t.Context(), false))
			} else {
				require.Error(t, err)
				require.Nil(t, second)
				require.Len(t, nativeExecutionRows(t, db), 1)
			}

			require.Equal(t, "open", runtimeUses(t, db)[0].State)
		})
	}
}

func TestAdmissionNativeExecutionDurableAnchorDoesNotCallSnapshotSource(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installNativeExecution(t, db)
	task := executionNativeTask(t, f)
	h, err := model.BeginNativeTaskExecution(t.Context(), db, task, model.NativeExecutionPoll, "")
	require.NoError(t, err)
	require.NoError(t, h.RecordReturn(t.Context(), false))

	restore := model.ConfigureChannelAdmissionRuntime(
		f.store.Executor,
		func(context.Context) (model.VerifiedDeploymentProof, error) {
			t.Fatal("existing anchor must not call snapshot source")
			return model.VerifiedDeploymentProof{}, nil
		},
	)
	defer restore()

	h, err = model.BeginNativeTaskExecution(t.Context(), db, task, model.NativeExecutionPoll, "")
	require.NoError(t, err)
	require.NoError(t, h.RecordReturn(t.Context(), false))
}

func TestAdmissionNativeExecutionFirstCoverageExpiresDuringWriteRollsBack(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installNativeExecution(t, db)
	task := executionNativeTask(t, f)
	deadline := time.Now().Add(time.Second)
	f.proof.ValidUntilMs = deadline.UnixMilli()
	hash, err := f.proof.Fingerprint()
	require.NoError(t, err)
	require.NoError(
		t,
		db.Model(&model.ChannelAdmissionProof{}).
			Where("id = ?", f.proof.ProofID).
			Update("fact_hash", hash).
			Error,
	)

	require.NoError(
		t,
		db.Callback().
			Create().
			After("gorm:create").
			Register("test:execution-proof-expiry", func(tx *gorm.DB) {
				if tx.Statement.Table == "channel_admission_task_executions" {
					if delay := time.Until(deadline.Add(30 * time.Millisecond)); delay > 0 {
						time.Sleep(delay)
					}
				}
			}),
	)
	defer func() {
		if err := db.
			Callback().Create().
			Remove("test:execution-proof-expiry"); err != nil {
			t.Error(err)
		}
	}()

	h, err := model.BeginNativeTaskExecution(t.Context(), db, task, model.NativeExecutionPoll, "")
	require.Error(t, err)
	require.Nil(t, h)
	require.Empty(t, nativeExecutionRows(t, db))
	require.Equal(t, "open", runtimeUses(t, db)[0].State)
}
