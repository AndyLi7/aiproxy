package model_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func installNativeExecution(t *testing.T, db *gorm.DB) {
	t.Helper()
	installNativeExecutionDraft(t, db, "v2")
}

func installNativeExecutionDraft(t *testing.T, db *gorm.DB, version string) {
	t.Helper()
	installExecutionJournal(t, db)
	raw, err := os.ReadFile(
		"../migrations/channel-native-execution-" + version + "/" + db.Name() + ".sql",
	)
	require.NoError(t, err)

	var lines []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}

	parts := strings.Split(strings.TrimSpace(strings.Join(lines, "\n")), ";")
	require.Equal(t, "BEGIN", strings.TrimSpace(parts[0]))
	require.Equal(t, "COMMIT", strings.TrimSpace(parts[len(parts)-2]))

	for range 2 {
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			for _, part := range parts[1 : len(parts)-2] {
				if e := tx.Exec(strings.TrimSpace(part)).Error; e != nil {
					return e
				}
			}

			return nil
		}))
	}
}

func nativeExecutionRows(t *testing.T, db *gorm.DB) []model.ChannelAdmissionTaskExecution {
	t.Helper()

	var rows []model.ChannelAdmissionTaskExecution
	require.NoError(t, db.Order("id").Find(&rows).Error)

	return rows
}

func executionNativeTask(t *testing.T, f *admissionFixture) *model.NativeTask {
	t.Helper()

	task, created, err := model.ReserveNativeTask(f.store.DB, runtimeNativeTask(f, "execution", 0))
	require.NoError(t, err)
	require.True(t, created)

	return task
}

func TestAdmissionNativeExecutionStorageAndStartBinding(t *testing.T) {
	for _, scenario := range []string{"absent", "relay-only", "installed", "missing-protocol", "missing-table", "wrong-version", "missing-column", "storage-generation", "stale-task", "stale-channel", "wrong-owner", "expired-owner"} {
		t.Run(scenario, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB

			task := executionNativeTask(t, f)
			if scenario == "relay-only" {
				installExecutionJournal(t, db)
			} else if scenario != "absent" {
				installNativeExecution(t, db)
			}

			owner := ""
			switch scenario {
			case "missing-protocol":
				require.NoError(
					t,
					db.Where("id = ?", 2).Delete(&model.ChannelAdmissionExecutionProtocol{}).Error,
				)
			case "missing-table":
				require.NoError(t, db.Migrator().DropTable(&model.ChannelAdmissionTaskExecution{}))
			case "wrong-version":
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionExecutionProtocol{}).
						Where("id = ?", 2).
						Update("version", 99).
						Error,
				)
			case "missing-column":
				require.NoError(
					t,
					db.Exec(
						"ALTER TABLE channel_admission_task_executions DROP COLUMN task_hash",
					).Error,
				)
			case "storage-generation":
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionControl{}).
						Where("id = ?", 1).
						Update("storage_generation", "restored").
						Error,
				)
			case "stale-task":
				task.BillingOperationID = "stale"
			case "stale-channel":
				require.NoError(
					t,
					db.Model(&model.Channel{}).
						Where("id = ?", 1).
						Update("base_url", "https://changed.invalid").
						Error,
				)
			case "wrong-owner":
				owner = "not-owner"
			case "expired-owner":
				owner = "owner"
				require.NoError(
					t,
					db.Model(&model.NativeTask{}).
						Where("id = ?", task.ID).
						Updates(map[string]any{"recovery_owner": owner, "recovery_until": time.Now().Add(-time.Second).Unix()}).
						Error,
				)
			}

			h, err := beginNativeExecution(
				context.Background(),
				db,
				task,
				model.NativeExecutionPoll,
				owner,
			)
			switch scenario {
			case "absent", "relay-only":
				require.NoError(t, err)
				require.Nil(t, h)
			case "installed":
				require.NoError(t, err)
				require.NotNil(t, h)
				require.Len(t, nativeExecutionRows(t, db), 1)
			default:
				require.Error(t, err)
				require.Nil(t, h)
			}

			require.Equal(t, "open", runtimeUses(t, db)[0].State)
		})
	}
}

func TestAdmissionNativeExecutionNestedCallsAndSubmissionRestamp(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installNativeExecution(t, db)
	task := executionNativeTask(t, f)
	outer, err := beginNativeExecution(
		context.Background(),
		db,
		task,
		model.NativeExecutionSubmit,
		"",
	)
	require.NoError(t, err)
	inner, err := beginNativeExecution(
		context.Background(),
		db,
		task,
		model.NativeExecutionBilling,
		"",
	)
	require.NoError(t, err)
	//nolint:staticcheck // Regression requires private authorization handles to serialize as an empty object.
	raw, err := json.Marshal(inner)
	require.NoError(t, err)
	require.Equal(t, "{}", string(raw))
	require.NoError(t, inner.RecordReturn(context.Background(), false))

	active := 0
	for _, r := range nativeExecutionRows(t, db) {
		if r.State == "active" {
			active++
		}

		require.Equal(t, "unproven", r.ContinuationState)
	}

	require.Equal(t, 1, active)
	// Existing business transition intentionally restamps creation on submission;
	// the immutable admission use, not this mutable deadline, binds the return.
	require.NoError(
		t,
		model.TransitionNativeSubmission(
			db,
			task.ID,
			task.GroupID,
			task.TokenID,
			"reserved",
			"submitting",
			"",
		),
	)
	require.NoError(t, outer.RecordReturn(context.Background(), false))
	require.NoError(t, outer.RecordReturn(context.Background(), false))
	require.Error(t, outer.RecordReturn(context.Background(), true))

	for _, r := range nativeExecutionRows(t, db) {
		require.Equal(t, "returned", r.State)
		require.Equal(t, "unproven", r.ContinuationState)
	}

	require.Equal(t, "open", runtimeUses(t, db)[0].State)
}

func TestAdmissionNativeExecutionLateReturnsAndOwnerLease(t *testing.T) {
	for _, scenario := range []string{"live", "changed-owner", "expired", "same-claim-extension", "shortened-lease", "changed-generation", "changed-channel", "storage-restored", "lost-capability"} {
		t.Run(scenario, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installNativeExecution(t, db)
			task := executionNativeTask(t, f)
			require.NoError(
				t,
				db.Model(&model.NativeTask{}).
					Where("id = ?", task.ID).
					Updates(map[string]any{"recovery_owner": "owner", "recovery_until": time.Now().Add(time.Minute).Unix()}).
					Error,
			)
			h, err := beginNativeExecution(
				context.Background(),
				db,
				task,
				model.NativeExecutionPoll,
				"owner",
			)
			require.NoError(t, err)

			row := nativeExecutionRows(t, db)[0]
			switch scenario {
			case "changed-owner":
				require.NoError(
					t,
					db.Model(&model.NativeTask{}).
						Where("id = ?", task.ID).
						Update("recovery_owner", "new-owner").
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
			case "same-claim-extension":
				require.NoError(
					t,
					db.Model(&model.NativeTask{}).
						Where("id = ?", task.ID).
						Update("recovery_until", time.Now().Add(2*time.Minute).Unix()).
						Error,
				)
			case "shortened-lease":
				require.NoError(
					t,
					db.Model(&model.NativeTask{}).
						Where("id = ?", task.ID).
						Update("recovery_until", time.Now().Add(30*time.Second).Unix()).
						Error,
				)
			case "changed-generation":
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionUse{}).
						Where("id = ?", row.UseID).
						Update("request_hash", strings.Repeat("b", 64)).
						Error,
				)
			case "changed-channel":
				require.NoError(
					t,
					db.Model(&model.Channel{}).
						Where("id = ?", 1).
						Update("base_url", "https://changed.invalid").
						Error,
				)
			case "storage-restored":
				require.NoError(
					t,
					db.Model(&model.ChannelAdmissionControl{}).
						Where("id = ?", 1).
						Update("storage_generation", "restored").
						Error,
				)
			case "lost-capability":
				h = &model.NativeTaskExecutionHandle{}
			}

			err = h.RecordReturn(context.Background(), false)

			current := nativeExecutionRows(t, db)[0]
			if scenario == "live" || scenario == "same-claim-extension" {
				require.NoError(t, err)
				require.Equal(t, "returned", current.State)
			} else {
				require.Error(t, err)
				require.Equal(t, "active", current.State)
				require.Zero(t, current.ReturnedAtMs)
			}
		})
	}
}

func TestAdmissionNativeExecutionLateReturnCannotTouchReplacementReservation(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installNativeExecution(t, db)
	old := executionNativeTask(t, f)
	handle, err := beginNativeExecution(
		context.Background(),
		db,
		old,
		model.NativeExecutionPoll,
		"",
	)
	require.NoError(t, err)
	evidence, err := model.CaptureNativeReservationRelease(db, old, "")
	require.NoError(t, err)
	released, err := model.ReleaseNativeReservation(
		db,
		old.ID,
		old.GroupID,
		old.TokenID,
		time.Now().Add(time.Second),
		evidence,
	)
	require.NoError(t, err)
	require.True(t, released)

	next, created, err := model.ReserveNativeTask(db, runtimeNativeTask(f, old.ID, 0))
	require.NoError(t, err)
	require.True(t, created)
	require.NotEqual(t, old.AdmissionGeneration, next.AdmissionGeneration)
	nextHandle, err := beginNativeExecution(
		context.Background(),
		db,
		next,
		model.NativeExecutionPoll,
		"",
	)
	require.NoError(t, err)
	require.NotNil(t, nextHandle)
	require.Error(t, handle.RecordReturn(context.Background(), false))

	for _, row := range nativeExecutionRows(t, db) {
		require.Equal(t, "active", row.State)
		require.Zero(t, row.ReturnedAtMs)
	}

	require.NoError(t, nextHandle.RecordReturn(context.Background(), false))
}

func TestAdmissionNativeExecutionLeaseExpiryDuringWriteRollsBack(t *testing.T) {
	for _, phase := range []string{"begin", "return"} {
		t.Run(phase, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installNativeExecution(t, db)
			task := executionNativeTask(t, f)
			deadline := time.Now().Add(2 * time.Second).Truncate(time.Second)
			require.NoError(
				t,
				db.Model(&model.NativeTask{}).
					Where("id = ?", task.ID).
					Updates(map[string]any{"recovery_owner": "owner", "recovery_until": deadline.Unix()}).
					Error,
			)

			var (
				handle *model.NativeTaskExecutionHandle
				err    error
			)
			if phase == "return" {
				handle, err = beginNativeExecution(
					context.Background(),
					db,
					task,
					model.NativeExecutionPoll,
					"owner",
				)
				require.NoError(t, err)
			}

			callback := func(tx *gorm.DB) {
				if tx.Statement.Table == "channel_admission_task_executions" {
					if wait := time.Until(deadline.Add(40 * time.Millisecond)); wait > 0 {
						time.Sleep(wait)
					}
				}
			}
			if phase == "begin" {
				require.NoError(
					t,
					db.Callback().
						Create().
						After("gorm:create").
						Register("test:native-execution-expiry", callback),
				)
				defer func() {
					if err := db.
						Callback().Create().
						Remove("test:native-execution-expiry"); err !=
						nil {
						t.Error(err)
					}
				}()

				_, err = beginNativeExecution(
					context.Background(),
					db,
					task,
					model.NativeExecutionPoll,
					"owner",
				)
			} else {
				require.NoError(
					t,
					db.Callback().
						Update().
						After("gorm:update").
						Register("test:native-execution-expiry", callback),
				)
				defer func() {
					if err := db.
						Callback().Update().
						Remove("test:native-execution-expiry"); err !=
						nil {
						t.Error(err)
					}
				}()

				err = handle.RecordReturn(context.Background(), false)
			}

			require.Error(t, err)

			rows := nativeExecutionRows(t, db)
			if phase == "begin" {
				require.Empty(t, rows)
			} else {
				require.Len(t, rows, 1)
				require.Equal(t, "active", rows[0].State)
				require.Zero(t, rows[0].ReturnedAtMs)
			}
		})
	}
}

func TestAdmissionNativeExecutionAbsentExtensionDoesNotLoadProof(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	task := executionNativeTask(t, f)

	restore := model.ConfigureChannelAdmissionRuntime(
		f.store.Executor,
		func(context.Context) (model.VerifiedDeploymentProof, error) {
			t.Fatal("v1 frozen work must not load execution proof")
			return model.VerifiedDeploymentProof{}, nil
		},
	)
	defer restore()

	handle, err := beginNativeExecution(
		context.Background(),
		f.store.DB,
		task,
		model.NativeExecutionPoll,
		"",
	)
	require.NoError(t, err)
	require.Nil(t, handle)
}

// Simulates the caller carrying the lease it observed before Begin waits for
// a task lock. Actual engine callers use the private claim context instead.
func beginNativeExecution(
	ctx context.Context,
	db *gorm.DB,
	task *model.NativeTask,
	kind model.NativeExecutionKind,
	owner string,
) (*model.NativeTaskExecutionHandle, error) {
	if owner == "" {
		return model.BeginNativeTaskExecution(ctx, db, task, kind, owner)
	}

	var snapshot model.NativeTask
	if err := db.Select("recovery_until").First(&snapshot, "id = ?", task.ID).Error; err != nil {
		return nil, err
	}

	return model.BeginNativeTaskExecution(ctx, db, task, kind, owner, snapshot.RecoveryUntil)
}

func TestAdmissionNativeExecutionProtectedExtensionKeepsClaimIdentity(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installNativeExecution(t, db)
	task := executionNativeTask(t, f)
	now := time.Now()
	claims, err := model.ClaimNativeRecovery(db, "same-worker", now, 1)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	claim := claims[0]
	outer, err := model.BeginNativeTaskExecution(
		context.Background(),
		db,
		task,
		model.NativeExecutionPoll,
		claim.RecoveryOwner,
		claim.RecoveryUntil,
	)
	require.NoError(t, err)
	// A's DB-clock/row-lock primitive implements the agreed interface. B's
	// actual archive call chain remains a separate combined integration check.
	extension := now.Add(11 * time.Minute)
	require.NoError(t, model.ExtendNativeRecovery(db, task.ID, claim.RecoveryOwner, extension))
	require.NoError(t, model.ExtendNativeRecovery(db, task.ID, claim.RecoveryOwner, extension))
	require.NoError(
		t,
		model.ExtendNativeRecovery(db, task.ID, claim.RecoveryOwner, now.Add(10*time.Minute)),
	)
	extended, readErr := model.GetNativeTask(db, task.ID, task.GroupID, task.TokenID)
	require.NoError(t, readErr)
	require.Equal(t, extension.Unix(), extended.RecoveryUntil)

	nested, err := model.BeginNativeTaskExecution(
		context.Background(),
		db,
		task,
		model.NativeExecutionDelivery,
		claim.RecoveryOwner,
		claim.RecoveryUntil,
	)
	require.NoError(t, err)
	require.NoError(t, nested.RecordReturn(context.Background(), false))
	require.NoError(t, outer.RecordReturn(context.Background(), false))
	require.NoError(t, model.ReleaseNativeRecovery(db, task.ID, claim.RecoveryOwner, now))
	replacement, err := model.ClaimNativeRecovery(db, "same-worker", now, 1)
	require.NoError(t, err)
	require.Len(t, replacement, 1)
	require.NotEqual(t, claim.RecoveryOwner, replacement[0].RecoveryOwner)
	h, err := model.BeginNativeTaskExecution(
		context.Background(),
		db,
		task,
		model.NativeExecutionPoll,
		claim.RecoveryOwner,
		claim.RecoveryUntil,
	)
	require.Error(t, err)
	require.Nil(t, h)
	require.Error(t, outer.RecordReturn(context.Background(), false))
	h, err = model.BeginNativeTaskExecution(
		context.Background(),
		db,
		task,
		model.NativeExecutionPoll,
		replacement[0].RecoveryOwner,
		replacement[0].RecoveryUntil,
	)
	require.NoError(t, err)
	require.NotNil(t, h)
}
