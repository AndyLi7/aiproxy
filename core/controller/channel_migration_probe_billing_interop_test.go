package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// Opt-in loopback-only fixture: actual Go native Submit/Poll, synthetic supplier
// and wallet, real HTTP context/prepare; the app supplies its actual billing route.
func TestMigrationProbeBillingInteropServer(t *testing.T) {
	dir := os.Getenv("B2_PROBE_BILLING_HTTP_FIXTURE_DIR")
	if dir == "" {
		t.Skip("local Go/app HTTP fixture")
	}
	require.True(t, filepath.IsAbs(dir))
	require.NoError(t, os.MkdirAll(dir, 0700))
	metaPath := filepath.Join(dir, "metadata.json")
	defer os.Remove(metaPath)
	defer os.Remove(filepath.Join(dir, "app.json"))
	f := newProbeHTTPFixture(t, true)
	f.billingReader = func(ctx context.Context, r model.MigrationProbeBillingRequest) (model.MigrationProbeBillingEvidence, error) {
		raw, e := os.ReadFile(filepath.Join(dir, "app.json"))
		if e != nil {
			return model.MigrationProbeBillingEvidence{}, model.ErrMigrationProbeMissingProvenance
		}
		var app struct {
			BaseURL string `json:"baseUrl"`
			Key     string `json:"key"`
		}
		if json.Unmarshal(raw, &app) != nil {
			return model.MigrationProbeBillingEvidence{}, model.ErrChannelAdmissionConflict
		}
		u, e := url.Parse(app.BaseURL)
		if e != nil || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
			return model.MigrationProbeBillingEvidence{}, model.ErrChannelAdmissionConflict
		}
		return balance.NewExternalHTTP(app.BaseURL, app.Key).ReadMigrationProbeBilling(ctx, r)
	}
	// The app must durably adopt the original created resource before the probe
	// changes source inventory. Do not fabricate a shadow_ready app journal.
	_, shadowRaw := f.call(t, "GET", "/api/channel-migrations/operations/http-probe-op/shadow", nil, config.AdminKey)
	require.Contains(t, string(shadowRaw), `"state":"ready"`)
	initial := map[string]any{"phase": "resource", "baseUrl": f.server.URL, "adminKey": config.AdminKey, "inventory": f.initialInventory, "resourceRequest": f.resourceRequest, "resourceReceipt": f.resourceReceipt, "probeRequest": f.body, "oldKey": "synthetic-old-key-id:synthetic-old-key-secret", "targetKey": "synthetic-target-key-id:synthetic-target-key-secret", "billingKey": "synthetic-billing-key-id:synthetic-billing-key-secret"}
	initialRaw, e := json.MarshalIndent(initial, "", "  ")
	require.NoError(t, e)
	require.NoError(t, os.WriteFile(metaPath, initialRaw, 0600))
	deadline := time.Now().Add(10 * time.Minute)
	for {
		if _, e = os.Stat(filepath.Join(dir, "app-prepared")); e == nil {
			break
		}
		if _, e = os.Stat(filepath.Join(dir, "stop")); e == nil {
			return
		}
		require.True(t, time.Now().Before(deadline), "app resource handshake timed out")
		time.Sleep(100 * time.Millisecond)
	}
	var raw []byte
	if os.Getenv("B2_PROBE_APP_DRIVEN") != "1" {
		_, raw = f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/probe", f.body, config.AdminKey)
	}
	var observed struct {
		Data model.MigrationProbeResult `json:"data"`
	}
	if os.Getenv("B2_PROBE_APP_DRIVEN") == "1" {
		// This mode never starts a probe: only the actual app command may POST.
		for {
			_, raw = f.call(t, "GET", "/api/channel-migrations/operations/http-probe-op/probe", nil, config.AdminKey)
			require.NoError(t, json.Unmarshal(raw, &observed))
			if observed.Data.Observation != nil && observed.Data.Observation.UseID != "" && observed.Data.Observation.TaskStatus == "pending" {
				break
			}
			if _, e = os.Stat(filepath.Join(dir, "stop")); e == nil {
				return
			}
			require.True(t, time.Now().Before(deadline), "app probe POST timed out")
			time.Sleep(100 * time.Millisecond)
		}
	} else {
		require.NoError(t, json.Unmarshal(raw, &observed))
	}
	require.NotNil(t, observed.Data.Observation)
	taskID := observed.Data.Observation.TaskID
	done, e := f.engine.Poll(t.Context(), taskID, f.body.GroupID, 1, func(context.Context, *model.NativeTask) (nativetask.Poller, error) {
		return probeCompletedPoller{}, nil
	}, func(_ context.Context, id string, index int, _ string) (ownedartifact.Receipt, error) {
		return ownedartifact.Receipt{Key: fmt.Sprintf("native-results/%s/%d/%s.bin", id, index, strings.Repeat("a", 64)), SHA256: strings.Repeat("b", 64), Size: 23}, nil
	})
	require.NoError(t, e)
	require.Equal(t, "completed", done.Status)
	var task model.NativeTask
	require.NoError(t, f.db.First(&task, "id=?", taskID).Error)
	input := model.MigrationProbeBillingContextRequest{BillingOperationID: task.BillingOperationID, TaskID: task.ID, GroupID: task.GroupID}
	got := f.store.ReadMigrationProbeBillingContext(t.Context(), input)
	require.Equal(t, "ready", got.State, got.Reason)
	meta := map[string]any{"phase": "completed", "baseUrl": f.server.URL, "adminKey": config.AdminKey, "inventory": f.initialInventory, "resourceRequest": f.resourceRequest, "resourceReceipt": f.resourceReceipt, "probeRequest": f.body, "probeObservation": observed.Data, "context": got, "quoteJson": task.PrepaymentQuoteJSON, "oldKey": "synthetic-old-key-id:synthetic-old-key-secret", "targetKey": "synthetic-target-key-id:synthetic-target-key-secret", "billingKey": "synthetic-billing-key-id:synthetic-billing-key-secret", "amounts": map[string]string{"costMicros": "600000", "customerDueMicros": "900000", "chargedMicros": "900000"}, "billingReceiptJson": task.BillingReceiptJSON}
	raw, e = json.MarshalIndent(meta, "", "  ")
	require.NoError(t, e)
	require.NoError(t, os.WriteFile(metaPath+".tmp", raw, 0600))
	require.NoError(t, os.Rename(metaPath+".tmp", metaPath))
	timeout := time.NewTimer(20 * time.Minute)
	defer timeout.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-timeout.C:
			t.Fatal("local fixture timed out")
		case <-ticker.C:
			if _, e = os.Stat(filepath.Join(dir, "stop")); e == nil {
				f.mu.Lock()
				require.Equal(t, 1, f.providerCalls)
				f.mu.Unlock()
				var tasks, uses int64
				require.NoError(t, f.db.Model(&model.NativeTask{}).Count(&tasks).Error)
				require.NoError(t, f.db.Model(&model.ChannelAdmissionUse{}).Count(&uses).Error)
				require.EqualValues(t, 1, tasks)
				require.EqualValues(t, 1, uses)
				return
			}
		}
	}
}
