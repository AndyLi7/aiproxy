package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestMigrationProbeBillingContextHTTPAuthAndActualTask(t *testing.T) {
	f := newProbeHTTPFixture(t)
	path := "/api/channel-migrations/probe-billing-context/read"
	for _, key := range []string{"", "wrong"} {
		code, _ := f.call(t, "POST", path, `{}`, key)
		require.Equal(t, 401, code)
	}
	code, _ := f.call(t, "POST", path+"?key=bad", `{}`, config.AdminKey)
	require.Equal(t, 401, code)
	for _, bad := range []string{`{}`, `null`, `{"taskId":"a","taskId":"b"}`, `{"billingOperationId":"bill","taskId":"a","groupId":"g","passed":true}`, strings.Repeat("x", 4097)} {
		code, _ := f.call(t, "POST", path, bad, config.AdminKey)
		require.Equal(t, 400, code)
	}
	code, raw := f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/probe", f.body, config.AdminKey)
	require.Equal(t, 200, code)
	var probe struct {
		Data model.MigrationProbeResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &probe))
	require.NotNil(t, probe.Data.Observation)
	input := model.MigrationProbeBillingContextRequest{BillingOperationID: "native:" + probe.Data.Observation.TaskID, TaskID: probe.Data.Observation.TaskID, GroupID: f.body.GroupID}
	code, raw = f.call(t, "POST", path, input, config.AdminKey)
	require.Equal(t, 200, code)
	var out struct {
		Data model.MigrationProbeBillingContextResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, "ready", out.Data.State, out.Data.Reason)
	require.False(t, out.Data.Context.BillingSettled)
	require.Equal(t, "synthetic-upstream-id", out.Data.Context.Binding.UpstreamTaskID)
	for _, secret := range []string{"synthetic-probe-shadow-key", "synthetic provider only", `"input"`, `"quoteJson"`} {
		require.NotContains(t, string(raw), secret)
	}
	restore := model.ConfigureChannelAdmissionRuntime(model.ChannelExecutorIdentity{}, nil)
	defer restore()
	originalLoader := f.store.LoadDeployment
	f.store.LoadDeployment = nil
	code, raw = f.call(t, "POST", path, input, config.AdminKey)
	require.Equal(t, 200, code)
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, "ready", out.Data.State, out.Data.Reason)
	_, err := f.store.CancelMigrationShadow(context.Background(), "http-probe-op", f.resourceReceipt.RequestHash, f.resourceReceipt.CheckpointHash)
	require.NoError(t, err)
	code, raw = f.call(t, "POST", path, input, config.AdminKey)
	require.Equal(t, 200, code)
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, "ready", out.Data.State, out.Data.Reason)
	require.Equal(t, "cancelled", out.Data.Context.ResourceState)
	// Restoring authorization cannot reactivate a cancelled resource.
	f.store.LoadDeployment = originalLoader
	code, raw = f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/prepare", map[string]any{"version": 1, "authorizationHash": probe.Data.Observation.AuthorizationHash}, config.AdminKey)
	require.Equal(t, 200, code)
	var rejected struct {
		Data model.MigrationProbeResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &rejected))
	require.Equal(t, "conflict", rejected.Data.State)
	require.Nil(t, rejected.Data.Observation)
	var prepared int64
	require.NoError(t, f.db.Model(&model.ChannelMigrationPrepared{}).Count(&prepared).Error)
	require.Zero(t, prepared)
	require.NotNil(t, migrationProbeDefaults().billing)
}
