package model_test

import (
	"context"
	"encoding/json"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"os"
	"strings"
	"testing"
	"time"
)

func installExecutionJournal(t *testing.T, db *gorm.DB) {
	t.Helper()
	raw, err := os.ReadFile("../migrations/channel-execution-v1/" + db.Dialector.Name() + ".sql")
	require.NoError(t, err)
	// pgx prepared statements accept one SQL statement at a time. Execute the
	// checked-in draft's statements on one real transaction/connection.
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}
	parts := strings.Split(strings.TrimSpace(strings.Join(lines, "\n")), ";")
	require.Equal(t, "BEGIN", strings.TrimSpace(parts[0]))
	require.Equal(t, "COMMIT", strings.TrimSpace(parts[len(parts)-2]))
	for replay := 0; replay < 2; replay++ {
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			for _, statement := range parts[1 : len(parts)-2] {
				if err := tx.Exec(strings.TrimSpace(statement)).Error; err != nil {
					return err
				}
			}
			return nil
		}))
	}

}
func relayExecution(t *testing.T, f *admissionFixture) (*model.ChannelAdmissionUse, *model.RelayExecutionHandle, error) {
	t.Helper()
	channel, err := model.GetChannelByID(1)
	require.NoError(t, err)
	return model.AdmitRelayExecution(context.Background(), channel, model.RelayAdmissionScope{GroupID: "g", TokenID: 1, BillingOperationID: "bill", RequestID: "same-client-id"})
}
func executionRows(t *testing.T, db *gorm.DB) []model.ChannelAdmissionExecution {
	t.Helper()
	var rows []model.ChannelAdmissionExecution
	require.NoError(t, db.Order("id").Find(&rows).Error)
	return rows
}
func TestAdmissionExecutionOptionalStorageAndAtomicity(t *testing.T) {
	for _, scenario := range []string{"absent", "installed", "journal-only", "protocol-only", "wrong-version", "missing-column", "missing-protocol-row", "write-failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			if scenario != "absent" {
				installExecutionJournal(t, db)
			}
			switch scenario {
			case "journal-only":
				require.NoError(t, db.Migrator().DropTable(&model.ChannelAdmissionExecutionProtocol{}))
			case "protocol-only":
				require.NoError(t, db.Migrator().DropTable(&model.ChannelAdmissionExecution{}))
			case "wrong-version":
				require.NoError(t, db.Model(&model.ChannelAdmissionExecutionProtocol{}).Where("id = ?", 1).Update("version", 2).Error)
			case "missing-column":
				require.NoError(t, db.Exec("ALTER TABLE channel_admission_executions DROP COLUMN continuation_state").Error)
			case "missing-protocol-row":
				require.NoError(t, db.Where("id = ?", 1).Delete(&model.ChannelAdmissionExecutionProtocol{}).Error)
			case "write-failure":
				require.NoError(t, db.Exec("CREATE TRIGGER reject_execution BEFORE INSERT ON channel_admission_executions BEGIN SELECT RAISE(ABORT, 'synthetic failure'); END").Error)
			}
			use, handle, err := relayExecution(t, f)
			if scenario == "absent" {
				require.NoError(t, err)
				require.NotNil(t, use)
				require.Nil(t, handle)
				require.Len(t, runtimeUses(t, db), 1)
			} else if scenario == "installed" {
				require.NoError(t, err)
				require.NotNil(t, handle)
				require.Len(t, executionRows(t, db), 1)
			} else {
				require.Error(t, err)
				require.Nil(t, use)
				require.Nil(t, handle)
				require.Empty(t, runtimeUses(t, db))
			}
		})
	}
}
func TestAdmissionExecutionReturnsDoNotCloseUses(t *testing.T) {
	for _, state := range []model.RelayExecutionReturn{model.RelayExecutionReturned, model.RelayExecutionErrorReturned, model.RelayExecutionAsyncReturned} {
		t.Run(string(state), func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			installExecutionJournal(t, f.store.DB)
			use, handle, err := relayExecution(t, f)
			require.NoError(t, err)
			raw, err := json.Marshal(handle)
			require.NoError(t, err)
			require.JSONEq(t, "{}", string(raw))
			initial := executionRows(t, f.store.DB)[0]
			require.Equal(t, "active", initial.State)
			require.Equal(t, "unproven", initial.ContinuationState)
			require.Equal(t, use.ID, initial.UseID)
			require.Equal(t, f.store.Executor.InstanceID, initial.InstanceID)
			_, err = f.store.Seal(context.Background(), f.seal("seal", 0, 1, 2, ""))
			require.NoError(t, err)
			require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionProof{}).Where("id = ?", f.proof.ProofID).Update("revoked", true).Error)
			require.NoError(t, handle.RecordReturn(context.Background(), state), "frozen return survives seal/revoked proof")
			require.NoError(t, handle.RecordReturn(context.Background(), state), "exact repeat is read-only")
			row := executionRows(t, f.store.DB)[0]
			require.Equal(t, string(state), row.State)
			require.GreaterOrEqual(t, row.ReturnedAtMs, row.StartedAtMs)
			require.Equal(t, "unproven", row.ContinuationState)
			retained := runtimeUses(t, f.store.DB)[0]
			require.Equal(t, "open", retained.State)
			require.Zero(t, retained.ClosedAtMs)
			require.Empty(t, retained.ClosureHash)
			require.Error(t, handle.RecordReturn(context.Background(), "drained"))
		})
	}
}
func TestAdmissionExecutionLateReturnCannotRewriteFacts(t *testing.T) {
	for _, scenario := range []string{"storage-generation", "use-replaced", "executor-replaced", "record-missing", "extension-missing", "state-conflict", "lost-capability"} {
		t.Run(scenario, func(t *testing.T) {
			f := runtimeAdmissionFixture(t)
			db := f.store.DB
			installExecutionJournal(t, db)
			use, handle, err := relayExecution(t, f)
			require.NoError(t, err)
			newer, _, err := relayExecution(t, f)
			require.NoError(t, err)
			require.NotEqual(t, use.ID, newer.ID)
			switch scenario {
			case "storage-generation":
				require.NoError(t, db.Model(&model.ChannelAdmissionControl{}).Where("id = ?", 1).Update("storage_generation", "restored").Error)
			case "use-replaced":
				require.NoError(t, db.Model(&model.ChannelAdmissionUse{}).Where("id = ?", use.ID).Update("request_hash", newer.ID).Error)
			case "executor-replaced":
				require.NoError(t, db.Model(&model.ChannelAdmissionExecution{}).Where("use_id = ?", use.ID).Update("instance_id", "different-worker").Error)
			case "record-missing":
				require.NoError(t, db.Where("use_id = ?", use.ID).Delete(&model.ChannelAdmissionExecution{}).Error)
			case "extension-missing":
				require.NoError(t, db.Migrator().DropTable(&model.ChannelAdmissionExecutionProtocol{}))
			case "state-conflict":
				require.NoError(t, handle.RecordReturn(context.Background(), model.RelayExecutionErrorReturned))
			case "lost-capability":
				handle = &model.RelayExecutionHandle{}
			}
			require.Error(t, handle.RecordReturn(context.Background(), model.RelayExecutionReturned))
			var next model.ChannelAdmissionExecution
			require.NoError(t, db.First(&next, "use_id = ?", newer.ID).Error)
			require.Equal(t, "active", next.State)
			require.Zero(t, next.ReturnedAtMs)
			for _, u := range runtimeUses(t, db) {
				require.Equal(t, "open", u.State)
			}
		})
	}
}

func TestAdmissionExecutionProofExpiresAfterJournalWriteRollsBack(t *testing.T) {
	f := runtimeAdmissionFixture(t)
	db := f.store.DB
	installExecutionJournal(t, db)
	f.proof.ValidUntilMs = time.Now().Add(350 * time.Millisecond).UnixMilli()
	hash, err := f.proof.Fingerprint()
	require.NoError(t, err)
	require.NoError(t, db.Model(&model.ChannelAdmissionProof{}).Where("id = ?", f.proof.ProofID).Update("fact_hash", hash).Error)
	require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:execution-proof-expiry", func(tx *gorm.DB) {
		if tx.Statement.Table == "channel_admission_executions" {
			time.Sleep(500 * time.Millisecond)
		}
	}))
	defer db.Callback().Create().Remove("test:execution-proof-expiry")
	use, handle, err := relayExecution(t, f)
	require.Error(t, err)
	require.Nil(t, use)
	require.Nil(t, handle)
	require.Empty(t, runtimeUses(t, db))
	require.Empty(t, executionRows(t, db))
}
