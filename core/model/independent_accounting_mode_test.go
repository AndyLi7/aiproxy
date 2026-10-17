package model_test

import (
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestIndependentAccountingPrepaidExclusionRequiresOperationBinding(t *testing.T) {
	db := asyncRenewalDB(t)
	info := claimDeadlineFixture(t, db, time.Now().Add(-time.Second))
	info.BillingOperationID = "usage-operation"
	require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID).UpdateColumn("billing_operation_id", info.BillingOperationID).Error)
	require.NoError(t, db.Model(&model.Log{}).Where("id=?", info.LogID).UpdateColumn("billing_operation_id", info.BillingOperationID).Error)
	require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", info.ImageTaskID).Updates(map[string]any{"prepayment_quote_json": `{"version":1}`, "billing_operation_id": "other-task-operation", "usage_id": info.ID}).Error)
	if !model.UsesImageAccountingClaim(info) {
		t.Error("mismatched task billing operation incorrectly grants prepaid exclusion")
	}
	err := model.UpdateAsyncUsageLogUsage(info, model.Usage{ImageOutputTokens: 1}, model.UsageContext{}, info.Price, model.Amount{UsedAmount: .3}, "USD", "frozen")
	var entry model.Log
	require.NoError(t, db.First(&entry, info.LogID).Error)
	if err == nil || entry.AsyncUsageStatus != model.AsyncUsageStatusPending {
		t.Errorf("expired mismatched task receipt accepted: err=%v status=%v amount=%v", err, entry.AsyncUsageStatus, entry.Amount.UsedAmount)
	}
}

func TestAccountingPrepaidEmptyOperationsDoNotExemptExpiredReceipt(t *testing.T) {
	db := asyncRenewalDB(t)
	info := claimDeadlineFixture(t, db, time.Now().Add(-time.Second))
	require.NoError(t, db.Model(&model.ImageTask{}).Where("id=?", info.ImageTaskID).Updates(map[string]any{"prepayment_quote_json": `{"version":1}`, "usage_id": info.ID}).Error)
	require.True(t, model.UsesImageAccountingClaim(info))
	require.Error(t, model.UpdateAsyncUsageLogUsage(info, model.Usage{ImageOutputTokens: 1}, model.UsageContext{}, info.Price, model.Amount{UsedAmount: .3}, "USD", "frozen"))
	var entry model.Log
	require.NoError(t, db.First(&entry, info.LogID).Error)
	require.Equal(t, model.AsyncUsageStatusPending, entry.AsyncUsageStatus)
	require.Zero(t, entry.Amount.UsedAmount)
}
