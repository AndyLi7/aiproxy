package nativetask

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestAdmissionNativeLostLeaseBeforeAbandonCannotRefund(t *testing.T) {
	e, p, _, provider := setup(t)
	activateNativeRuntime(t, e, &p)
	e.Wallet = &scriptedWallet{errs: map[string]error{"admit": errors.New("unknown")}}
	_, err := e.Submit(context.Background(), "lost-before-abandon", "g", 1, body, p)
	require.Error(t, err)
	past := time.Now().Add(-20 * time.Minute)
	require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "lost-before-abandon").Updates(map[string]any{"created_at": past, "updated_at": past}).Error)
	wallet := &appWallet{held: true, status: "pending", submission: "not_submitted", createdAt: past}
	e.Wallet = wallet
	e.AbandonReservations = true
	replaced := false
	require.NoError(t, e.DB.Callback().Query().After("gorm:after_query").Register("test:lose-after-receipt", func(tx *gorm.DB) {
		if replaced {
			return
		}
		task, ok := tx.Statement.Dest.(*model.NativeTask)
		if !ok || task.ID != "lost-before-abandon" || task.BillingReceiptJSON == "" || task.RecoveryOwner != "old-worker" {
			return
		}
		replaced = true
		result := e.DB.Model(&model.NativeTask{}).Where("id = ?", task.ID).UpdateColumns(map[string]any{"recovery_owner": "replacement-worker", "recovery_until": time.Now().Add(time.Minute).Unix()})
		require.NoError(t, result.Error)
	}))
	defer e.DB.Callback().Query().Remove("test:lose-after-receipt")
	report, recoverErr := e.RecoverOnce(context.Background(), "old-worker", time.Now(), nil, nil)
	require.True(t, replaced)
	current, err := model.GetNativeTask(e.DB, "lost-before-abandon", "g", 1)
	require.NoError(t, err)
	t.Logf("report=%+v error=%v status=%s errorCode=%s owner=%s walletActions=%v", report, recoverErr, current.Status, current.ErrorCode, current.RecoveryOwner, wallet.actions())
	require.Zero(t, provider.calls)
	require.Equal(t, "reserved", current.Status, "worker that lost its lease must not abandon/refund")
	require.Equal(t, []string{"get"}, wallet.actions())
}

func TestAdmissionNativeAbandonChecksEveryTerminalDecision(t *testing.T) {
	for _, kind := range []string{"live", "lease-expired", "new-generation", "receipt-changed", "expires-in-write", "proof-withdrawn"} {
		t.Run(kind, func(t *testing.T) {
			e, p, _, provider := setup(t)
			store, _ := activateNativeRuntime(t, e, &p)
			e.Wallet = &scriptedWallet{errs: map[string]error{"admit": errors.New("unknown")}}
			_, err := e.Submit(context.Background(), "abandon-guard", "g", 1, body, p)
			require.Error(t, err)
			past := time.Now().Add(-20 * time.Minute)
			require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "abandon-guard").Updates(map[string]any{"created_at": past, "updated_at": past}).Error)
			wallet := &appWallet{held: true, status: "pending", submission: "not_submitted", createdAt: past}
			e.Wallet = wallet
			e.AbandonReservations = true
			injected := false
			reads := 0
			require.NoError(t, e.DB.Callback().Query().After("gorm:after_query").Register("test:abandon-race", func(tx *gorm.DB) {
				if injected || kind == "live" || kind == "expires-in-write" {
					return
				}
				task, ok := tx.Statement.Dest.(*model.NativeTask)
				if !ok || task.ID != "abandon-guard" || task.BillingReceiptJSON == "" || task.RecoveryOwner != "worker" {
					return
				}
				reads++
				if kind == "receipt-changed" && reads < 2 {
					return
				}
				injected = true
				switch kind {
				case "lease-expired":
					require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", task.ID).UpdateColumn("recovery_until", time.Now().Add(-time.Second).Unix()).Error)
				case "proof-withdrawn":
					restore := model.ConfigureChannelAdmissionRuntime(store.Executor, nil)
					t.Cleanup(restore)
					require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", task.ID).UpdateColumn("recovery_owner", "replacement").Error)
				case "receipt-changed":
					require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", task.ID).UpdateColumn("billing_receipt_json", `{"status":"pending","submissionStatus":"accepted"}`).Error)
				case "new-generation":
					evidence, captureErr := model.CaptureNativeReservationRelease(e.DB, task, "worker")
					require.NoError(t, captureErr)
					released, releaseErr := model.ReleaseNativeReservation(e.DB, task.ID, task.GroupID, task.TokenID, time.Now().Add(time.Hour), evidence)
					require.NoError(t, releaseErr)
					require.True(t, released)
					replacement := *task
					replacement.AdmissionIdentityHash = p.AdmissionIdentityHash
					_, created, reserveErr := model.ReserveNativeTask(e.DB, replacement)
					require.NoError(t, reserveErr)
					require.True(t, created)
					// Even identical creation time and owner must not replace generation.
					require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", task.ID).UpdateColumns(map[string]any{"billing_receipt_json": task.BillingReceiptJSON, "recovery_owner": "worker", "recovery_until": time.Now().Add(time.Minute).Unix()}).Error)
				}
			}))
			defer e.DB.Callback().Query().Remove("test:abandon-race")
			require.NoError(t, e.DB.Callback().Update().After("gorm:update").Register("test:expire-abandon-write", func(tx *gorm.DB) {
				if kind != "expires-in-write" || injected || tx.Statement.Table != "native_tasks" {
					return
				}
				change, ok := tx.Statement.Dest.(map[string]any)
				if !ok || change["error_code"] != model.NotStartedCode {
					return
				}
				injected = true
				require.NoError(t, tx.Session(&gorm.Session{NewDB: true}).Exec("UPDATE native_tasks SET recovery_until = ? WHERE id = ?", time.Now().Add(-time.Second).Unix(), "abandon-guard").Error)
			}))
			defer e.DB.Callback().Update().Remove("test:expire-abandon-write")
			report, _ := e.RecoverOnce(context.Background(), "worker", time.Now(), nil, nil)
			current, err := model.GetNativeTask(e.DB, "abandon-guard", "g", 1)
			require.NoError(t, err)
			require.Zero(t, provider.calls)
			if kind == "live" {
				require.Equal(t, 1, report.Abandoned)
				require.Equal(t, "failed", current.Status)
				require.Equal(t, model.NotStartedCode, current.ErrorCode)
				require.Equal(t, []string{"get", "get", "settle:failed/not_dispatched", "get"}, wallet.actions())
				again, err := e.RecoverOnce(context.Background(), "worker", time.Now().Add(time.Hour), nil, nil)
				require.NoError(t, err)
				require.Zero(t, again.Claimed)
			} else {
				require.True(t, injected)
				require.Zero(t, report.Abandoned)
				require.Equal(t, "reserved", current.Status)
				require.Equal(t, []string{"get"}, wallet.actions())
			}
		})
	}
}
