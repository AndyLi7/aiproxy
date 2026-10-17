package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestMigrationProbeBillingHTTPConnectedRetryNoRegeneration(t *testing.T) {
	f := newProbeHTTPFixture(t)
	_, raw := f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/probe", f.body, config.AdminKey)
	var response struct {
		Data model.MigrationProbeResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &response))
	obs := response.Data.Observation
	require.NotNil(t, obs)
	_, e := f.engine.Poll(t.Context(), obs.TaskID, f.body.GroupID, 1, func(context.Context, *model.NativeTask) (nativetask.Poller, error) {
		return probeCompletedPoller{}, nil
	}, func(_ context.Context, id string, index int, _ string) (ownedartifact.Receipt, error) {
		return ownedartifact.Receipt{Key: fmt.Sprintf("native-results/%s/%d/%s.bin", id, index, strings.Repeat("a", 64)), SHA256: strings.Repeat("b", 64), Size: 23}, nil
	})
	require.NoError(t, e)
	var original model.NativeTask
	require.NoError(t, f.db.First(&original, "id=?", obs.TaskID).Error)
	var calls atomic.Int32
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/internal/model-ops/migration-probe-billing", r.URL.Path)
		require.Equal(t, "Bearer synthetic-app-key", r.Header.Get("Authorization"))
		var binding model.MigrationProbeBillingRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&binding))
		// The callback can perform an actual second HTTP->DB read while Finalize
		// awaits the app. Holding the gateway control lock over this call would hang.
		_, raw := f.call(t, "POST", "/api/channel-migrations/probe-billing-context/read", model.MigrationProbeBillingContextRequest{BillingOperationID: binding.BillingOperationID, TaskID: binding.TaskID, GroupID: binding.GroupID}, config.AdminKey)
		var facts struct {
			Data model.MigrationProbeBillingContextResult `json:"data"`
		}
		require.NoError(t, json.Unmarshal(raw, &facts))
		require.Equal(t, "ready", facts.Data.State)
		require.Equal(t, binding, facts.Data.Context.Binding)
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"version": 1, "source": "app-migration-probe-billing-v1", "state": "ready", "reason": "", "evidence": map[string]any{"binding": binding, "evidenceId": "same-event", "provenanceHash": strings.Repeat("d", 64), "accountMembershipEvidenceHash": strings.Repeat("e", 64), "accountMembershipEvidenceKind": "fal-workspace-key-membership-v1", "currency": "USD", "costMicros": "600000", "customerDueMicros": "900000", "chargedMicros": "900000", "settledAtMs": fmt.Sprint(time.Now().Add(-time.Second).UnixMilli())}}})
	}))
	defer app.Close()
	old := balance.Default
	balance.Default = balance.NewExternalHTTP(app.URL, "synthetic-app-key")
	defer func() { balance.Default = old }()
	f.billingReader = migrationProbeDefaults().billing
	body := map[string]any{"version": 1, "authorizationHash": obs.AuthorizationHash}
	_, raw = f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/prepare", body, config.AdminKey)
	require.Contains(t, string(raw), `"state":"unavailable"`)
	var prepared int64
	require.NoError(t, f.db.Model(&model.ChannelMigrationPrepared{}).Count(&prepared).Error)
	require.Zero(t, prepared)
	_, raw = f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/prepare", body, config.AdminKey)
	require.Contains(t, string(raw), `"state":"ready"`)
	require.Contains(t, string(raw), `"state":"prepared"`)
	require.EqualValues(t, 2, calls.Load())
	require.Equal(t, 1, f.providerCalls)
	var after model.NativeTask
	require.NoError(t, f.db.First(&after, "id=?", obs.TaskID).Error)
	require.Equal(t, original, after)
	var barriers int64
	require.NoError(t, f.db.Model(&model.ChannelAdmissionBarrier{}).Count(&barriers).Error)
	require.Zero(t, barriers)
}
