package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func prepareHTTPProbeForCutover(t *testing.T, f *probeHTTPFixture) model.MigrationProbeCutoverBinding {
	t.Helper()
	_, raw := f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/probe", f.body, config.AdminKey)
	var observed struct {
		Data model.MigrationProbeResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &observed))
	require.NotNil(t, observed.Data.Observation, string(raw))
	obs := observed.Data.Observation
	_, err := f.engine.Poll(t.Context(), obs.TaskID, f.body.GroupID, 1, func(context.Context, *model.NativeTask) (nativetask.Poller, error) {
		return probeCompletedPoller{}, nil
	}, func(_ context.Context, id string, index int, _ string) (ownedartifact.Receipt, error) {
		return ownedartifact.Receipt{Key: fmt.Sprintf("native-results/%s/%d/%s.bin", id, index, strings.Repeat("a", 64)), SHA256: strings.Repeat("b", 64), Size: 23}, nil
	})
	require.NoError(t, err)
	f.billingReader = func(_ context.Context, b model.MigrationProbeBillingRequest) (model.MigrationProbeBillingEvidence, error) {
		return model.MigrationProbeBillingEvidence{Binding: b, EvidenceID: "cutover-synthetic-bill", ProvenanceHash: strings.Repeat("d", 64), AccountMembershipEvidenceHash: strings.Repeat("e", 64), AccountMembershipEvidenceKind: "fal-workspace-key-membership-v1", Currency: "USD", CostMicros: 600000, CustomerDueMicros: 900000, ChargedMicros: 900000, SettledAtMs: time.Now().Add(-time.Second).UnixMilli()}, nil
	}
	_, raw = f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/prepare", map[string]any{"version": 1, "authorizationHash": obs.AuthorizationHash}, config.AdminKey)
	var read struct {
		Data model.MigrationReadResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &read))
	require.Equal(t, "ready", read.Data.State, string(raw))
	require.NotNil(t, read.Data.Readback)
	receipt := read.Data.Readback.Receipt
	return model.MigrationProbeCutoverBinding{Version: 1, AuthorizationHash: obs.AuthorizationHash, RequestHash: receipt.RequestHash, ProbeFactHash: receipt.ProbeFactHash, StorageGeneration: receipt.StorageGeneration}
}
func assertCutoverNotApplied(t *testing.T, f *probeHTTPFixture) {
	t.Helper()
	var n int64
	require.NoError(t, f.db.Model(&model.ChannelAdmissionBarrier{}).Count(&n).Error)
	require.Zero(t, n)
	var row model.ChannelMigrationPrepared
	require.NoError(t, f.db.First(&row, "operation_id=?", "http-probe-op").Error)
	require.Equal(t, "prepared", row.State)
	var target model.Channel
	require.NoError(t, f.db.First(&target, f.body.Command.Seal.To.ChannelID).Error)
	require.Equal(t, model.ChannelStatusDisabled, target.Status)
}
func TestMigrationCutoverHTTPReplayAndReadback(t *testing.T) {
	f := newProbeHTTPFixture(t)
	b := prepareHTTPProbeForCutover(t, f)
	// Discarding the actual HTTP result simulates lost delivery, not lost DB commit.
	_, _ = f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/cutover", b, config.AdminKey)
	_, raw := f.call(t, "GET", "/api/channel-migrations/operations/http-probe-op", nil, config.AdminKey)
	require.Contains(t, string(raw), `"state":"committed"`)
	first := string(raw)
	_, raw = f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/cutover", b, config.AdminKey)
	require.Equal(t, first, string(raw))
	require.Equal(t, 1, f.providerCalls)
	var n int64
	require.NoError(t, f.db.Model(&model.NativeTask{}).Count(&n).Error)
	require.EqualValues(t, 1, n)
	// Loss of online issuer removes write capability but not historical GET facts.
	restore := model.ConfigureChannelAdmissionRuntime(model.ChannelExecutorIdentity{}, nil)
	defer restore()
	_, raw = f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/cutover", b, config.AdminKey)
	require.Contains(t, string(raw), `"state":"unavailable"`)
	_, raw = f.call(t, "GET", "/api/channel-migrations/operations/http-probe-op", nil, config.AdminKey)
	require.Equal(t, first, string(raw))
}
func TestMigrationCutoverHTTPRejectsChangedFacts(t *testing.T) {
	for _, mode := range []string{"authorization", "request", "probe", "storage", "task", "revoked", "cancelled", "issuer"} {
		t.Run(mode, func(t *testing.T) {
			f := newProbeHTTPFixture(t)
			b := prepareHTTPProbeForCutover(t, f)
			switch mode {
			case "authorization":
				b.AuthorizationHash = strings.Repeat("0", 64)
			case "request":
				b.RequestHash = strings.Repeat("0", 64)
			case "probe":
				b.ProbeFactHash = strings.Repeat("0", 64)
			case "storage":
				b.StorageGeneration = "other"
			case "task":
				require.NoError(t, f.db.Model(&model.NativeTask{}).Where("1=1").UpdateColumn("upstream_id", "changed").Error)
			case "revoked":
				require.NoError(t, f.db.Model(&model.ChannelMigrationProbeRecord{}).Where("1=1").UpdateColumn("revoked", true).Error)
			case "cancelled":
				var row model.ChannelMigrationShadowResource
				require.NoError(t, f.db.First(&row, "operation_id=?", "http-probe-op").Error)
				_, e := f.store.CancelMigrationShadow(t.Context(), row.OperationID, row.RequestHash, row.CheckpointHash)
				require.NoError(t, e)
			case "issuer":
				restore := model.ConfigureChannelAdmissionRuntime(model.ChannelExecutorIdentity{}, nil)
				defer restore()
			}
			_, raw := f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/cutover", b, config.AdminKey)
			require.NotContains(t, string(raw), `"state":"committed"`)
			if mode != "cancelled" {
				assertCutoverNotApplied(t, f)
			}
		})
	}
}
func TestMigrationCutoverHTTPAuthAndStrictBody(t *testing.T) {
	f := newProbeHTTPFixture(t)
	b := prepareHTTPProbeForCutover(t, f)
	code, _ := f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/cutover", b, "")
	require.NotEqual(t, 200, code)
	raw, _ := json.Marshal(b)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	body["proof"] = true
	code, _ = f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/cutover", body, config.AdminKey)
	require.Equal(t, 400, code)
	code, _ = f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/cutover?key="+config.AdminKey, b, config.AdminKey)
	require.Equal(t, 401, code)
	assertCutoverNotApplied(t, f)
}

func TestMigrationCutoverHTTPRejectsAmbiguousJSON(t *testing.T) {
	f := newProbeHTTPFixture(t)
	b := prepareHTTPProbeForCutover(t, f)
	valid, e := json.Marshal(b)
	require.NoError(t, e)
	for _, raw := range []string{string(valid) + "{}", strings.Replace(string(valid), `"version":1`, `"version":1,"version":1`, 1), strings.Replace(string(valid), `"version"`, `"Version"`, 1), strings.Repeat(" ", 4097)} {
		req, e := http.NewRequest("POST", f.server.URL+"/api/channel-migrations/operations/http-probe-op/cutover", bytes.NewBufferString(raw))
		require.NoError(t, e)
		req.Header.Set("Authorization", "Bearer "+config.AdminKey)
		res, e := http.DefaultClient.Do(req)
		require.NoError(t, e)
		_, e = io.Copy(io.Discard, res.Body)
		require.NoError(t, e)
		res.Body.Close()
		require.Equal(t, 400, res.StatusCode)
		require.Equal(t, "no-store", res.Header.Get("Cache-Control"))
	}
	assertCutoverNotApplied(t, f)
}

func TestMigrationCutoverHTTPExpiredReplayOnlyReadsFacts(t *testing.T) {
	f := newProbeHTTPFixture(t)
	proof, e := f.store.LoadDeployment(t.Context())
	require.NoError(t, e)
	deadline := time.Now().Add(10 * time.Second)
	proof.ValidUntilMs = deadline.UnixMilli()
	proof.RevocationFreshUntilMs = proof.ValidUntilMs
	hash, e := proof.Fingerprint()
	require.NoError(t, e)
	require.NoError(t, f.db.Model(&model.ChannelAdmissionProof{}).Where("id=?", proof.ProofID).UpdateColumn("fact_hash", hash).Error)
	f.store.LoadDeployment = func(context.Context) (model.VerifiedDeploymentProof, error) { return proof, nil }
	restore := model.ConfigureChannelAdmissionRuntime(f.store.Executor, f.store.LoadDeployment)
	defer restore()
	b := prepareHTTPProbeForCutover(t, f)
	_, raw := f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/cutover", b, config.AdminKey)
	require.Contains(t, string(raw), `"state":"committed"`)
	first := string(raw)
	if wait := time.Until(deadline.Add(30 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	_, raw = f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/cutover", b, config.AdminKey)
	require.NotContains(t, string(raw), `"state":"committed"`)
	_, raw = f.call(t, "GET", "/api/channel-migrations/operations/http-probe-op", nil, config.AdminKey)
	require.Equal(t, first, string(raw))
	require.Equal(t, 1, f.providerCalls)
}
func TestMigrationCutoverHTTPRejectsSupersededHead(t *testing.T) {
	f := newProbeHTTPFixture(t)
	b := prepareHTTPProbeForCutover(t, f)
	_, raw := f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/cutover", b, config.AdminKey)
	require.Contains(t, string(raw), `"state":"committed"`)
	// Model tests cover a complete next forward migration. At this HTTP boundary,
	// a changed persistent head must never be mistaken for this operation's ACK.
	require.NoError(t, f.db.Model(&model.ChannelAdmissionFamily{}).Where("operation_id=?", "http-probe-op").UpdateColumn("operation_id", "later-operation").Error)
	_, raw = f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/cutover", b, config.AdminKey)
	require.Contains(t, string(raw), `"state":"conflict"`)
	_, raw = f.call(t, "GET", "/api/channel-migrations/operations/http-probe-op", nil, config.AdminKey)
	require.Contains(t, string(raw), `"state":"conflict"`)
	require.Equal(t, 1, f.providerCalls)
}

func TestMigrationCutoverHTTPDoesNotAdoptHistoricalSeal(t *testing.T) {
	f := newProbeHTTPFixture(t)
	b := prepareHTTPProbeForCutover(t, f)
	var run model.ChannelMigrationProbeRun
	require.NoError(t, f.db.First(&run, "operation_id=?", "http-probe-op").Error)
	var fact model.VerifiedMigrationProbe
	require.NoError(t, json.Unmarshal([]byte(run.EvidenceJSON), &fact))
	command, ok := probeCommand(f.body.Command)
	require.True(t, ok)
	local := *f.store
	local.LoadMigrationProbe = func(context.Context, model.ChannelMigrationRequest) (model.VerifiedMigrationProbe, error) {
		return fact, nil
	}
	_, e := local.SealPrepared(t.Context(), command)
	require.NoError(t, e)
	_, raw := f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/cutover", b, config.AdminKey)
	require.Contains(t, string(raw), `"state":"conflict"`)
	var row model.ChannelMigrationPrepared
	require.NoError(t, f.db.First(&row, "operation_id=?", "http-probe-op").Error)
	require.Equal(t, "sealed", row.State)
	var target model.Channel
	require.NoError(t, f.db.First(&target, command.Seal.To.ChannelID).Error)
	require.Equal(t, model.ChannelStatusDisabled, target.Status)
}
