package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type probeHTTPWallet struct {
	t         *testing.T
	db        *gorm.DB
	mu        sync.Mutex
	actions   []string
	delivered bool
}

func (w *probeHTTPWallet) Prepayment(_ context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var uses, tasks, active int64
	require.NoError(w.t, w.db.Model(&model.ChannelAdmissionUse{}).Where("state='open'").Count(&uses).Error)
	require.EqualValues(w.t, 1, uses)
	require.NoError(w.t, w.db.Model(&model.NativeTask{}).Count(&tasks).Error)
	require.EqualValues(w.t, 1, tasks)
	require.NoError(w.t, w.db.Model(&model.ChannelAdmissionTaskExecution{}).Where("state='active'").Count(&active).Error)
	require.Positive(w.t, active)
	w.actions = append(w.actions, c.Action)
	if c.Action == "delivered" {
		w.delivered = true
	}
	r := balance.PrepaymentReceipt{ID: c.BillingOperationID, Claimed: c.Action == "begin_attempt", Status: "pending", SubmissionStatus: json.RawMessage(`"accepted"`)}
	if w.delivered {
		n := int64(900000)
		r.Status = "settled"
		r.ChargedMicros = &n
	}
	return r, nil
}

type probeHTTPProvider struct{ url string }

func (p probeHTTPProvider) SubmitNative(ctx context.Context, _ string, input []byte) (string, error) {
	r, e := http.NewRequestWithContext(ctx, "POST", p.url, bytes.NewReader(input))
	if e != nil {
		return "", e
	}
	res, e := http.DefaultClient.Do(r)
	if e != nil {
		return "", e
	}
	defer res.Body.Close()
	body, e := io.ReadAll(res.Body)
	return string(body), e
}

type probeHTTPFixture struct {
	db                *gorm.DB
	store             *model.ChannelAdmissionStore
	body              migrationProbeBody
	engine            *nativetask.Engine
	wallet            *probeHTTPWallet
	server            *httptest.Server
	providerCalls     int
	loseProviderReply bool
	mu                sync.Mutex
	deps              migrationProbeDependencies
	billingReader     model.MigrationProbeBillingReader
	initialInventory  model.MigrationInventory
	resourceRequest   model.MigrationShadowRequest
	resourceReceipt   *model.MigrationShadowReceipt
}

func newProbeHTTPFixture(t *testing.T, attribution ...bool) *probeHTTPFixture {
	t.Helper()
	db, e := model.OpenSQLite(filepath.Join(t.TempDir(), "probe.db"))
	require.NoError(t, e)
	return newProbeHTTPFixtureDB(t, db, attribution...)
}

func newProbeHTTPFixtureDB(t *testing.T, db *gorm.DB, attribution ...bool) *probeHTTPFixture {
	t.Helper()
	var e error
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.ModelConfig{}, &model.Group{}, &model.Token{}, &model.NativeTask{}, &model.ChannelAdmissionControl{}, &model.ChannelAdmissionProof{}, &model.ChannelAdmissionFamily{}, &model.ChannelAdmissionBarrier{}, &model.ChannelAdmissionUse{}, &model.ChannelMigrationProtocol{}, &model.ChannelMigrationPrepared{}, &model.ChannelMigrationProbeRecord{}, &model.ChannelMigrationShadowProtocol{}, &model.ChannelMigrationShadowResource{}, &model.ChannelMigrationProbeRuntimeProtocol{}, &model.ChannelMigrationProbeRun{}, &model.ChannelAdmissionExecutionProtocol{}, &model.ChannelAdmissionExecution{}, &model.ChannelAdmissionTaskExecution{}))
	for _, r := range []any{&model.ChannelMigrationProtocol{ID: 1, Version: 1}, &model.ChannelMigrationShadowProtocol{ID: 1, Version: 2}, &model.ChannelMigrationProbeRuntimeProtocol{ID: 1, Version: 1}, &model.ChannelAdmissionExecutionProtocol{ID: 1, Version: 1}, &model.ChannelAdmissionExecutionProtocol{ID: 2, Version: model.NativeExecutionProtocolVersion}} {
		require.NoError(t, db.Create(r).Error)
	}
	proof := model.VerifiedDeploymentProof{ProofID: "synthetic-probe-http", Epoch: "probe-http-epoch", Sequence: 1, StorageGeneration: "probe-http-storage", ExecutorFenceHash: strings.Repeat("d", 64), IssuedAtMs: time.Now().Add(-time.Minute).UnixMilli(), ValidUntilMs: time.Now().Add(5 * time.Minute).UnixMilli(), RevocationFreshUntilMs: time.Now().Add(5 * time.Minute).UnixMilli()}
	if len(attribution) > 0 && attribution[0] {
		proof.ValidUntilMs = time.Now().Add(25 * time.Minute).UnixMilli()
		proof.RevocationFreshUntilMs = proof.ValidUntilMs
	}
	executor := model.ChannelExecutorIdentity{ServiceID: "local-test-gateway", InstanceID: "local-test-instance", BuildSHA: strings.Repeat("f", 64), Epoch: proof.Epoch}
	proof.Executors = []model.ChannelExecutorIdentity{executor}
	raw, _ := json.Marshal(proof.Executors)
	sum := sha256.Sum256(raw)
	proof.MembershipHash = hex.EncodeToString(sum[:])
	hash, e := proof.Fingerprint()
	require.NoError(t, e)
	require.NoError(t, db.Create(&model.ChannelAdmissionControl{ID: 1, Version: 1, Epoch: proof.Epoch, EpochSequence: 1, StorageGeneration: proof.StorageGeneration, MembershipHash: proof.MembershipHash, ExecutorFenceHash: proof.ExecutorFenceHash, ActiveProofID: proof.ProofID}).Error)
	require.NoError(t, db.Create(&model.ChannelAdmissionProof{ID: proof.ProofID, FactHash: hash, ConsumedSequence: 1}).Error)
	store := &model.ChannelAdmissionStore{DB: db, LogDB: db, Executor: executor, LoadDeployment: func(context.Context) (model.VerifiedDeploymentProof, error) { return proof, nil }}
	oldDB, oldLog, oldKey := model.DB, model.LogDB, config.AdminKey
	model.DB, model.LogDB = db, db
	store.MigrationFeatures = gatewayRuntimeFeatures()
	config.AdminKey = "synthetic-probe-admin"
	restore := model.ConfigureChannelAdmissionRuntime(executor, store.LoadDeployment)
	t.Cleanup(func() {
		restore()
		model.DB, model.LogDB = oldDB, oldLog
		config.AdminKey = oldKey
		p, _ := db.DB()
		_ = p.Close()
	})
	require.NoError(t, db.Create(&model.Group{ID: "internal-probe", Status: model.GroupStatusInternal}).Error)
	require.NoError(t, db.Create(&model.Token{ID: 1, GroupID: "internal-probe", Status: model.TokenStatusEnabled, Name: "probe"}).Error)
	oldProviderKey := "synthetic-preparation-old"
	if len(attribution) > 0 && attribution[0] {
		oldProviderKey = "synthetic-old-key-id:synthetic-old-key-secret"
	}
	ch := model.Channel{ID: 11, Name: "Primary [shadow v1-logical]", Type: model.ChannelTypeFal, Key: oldProviderKey, BaseURL: "https://queue.fal.run", Status: 1, Models: []string{"test/native::text-to-video"}, ModelMapping: map[string]string{"test/native::text-to-video": "fal-ai/fixture"}, Sets: []string{"default"}, Priority: 1, Configs: model.ChannelConfigs{"outbound_policy": "public_only"}}
	require.NoError(t, db.Create(&ch).Error)
	raw, e = os.ReadFile("../model/testdata/migration-inventory/publisher-builder.json")
	require.NoError(t, e)
	var pub struct {
		Config struct {
			Config json.RawMessage `json:"config"`
		} `json:"config"`
	}
	require.NoError(t, json.Unmarshal(raw, &pub))
	if len(attribution) > 0 && attribution[0] {
		var cfg map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(pub.Config.Config, &cfg))
		var binding map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(cfg["x_token_platform_native_result_v1"], &binding))
		digest := sha256.Sum256([]byte(oldProviderKey))
		binding["keyFingerprint"], _ = json.Marshal(hex.EncodeToString(digest[:]))
		cfg["x_token_platform_native_result_v1"], _ = json.Marshal(binding)
		pub.Config.Config, _ = json.Marshal(cfg)
	}
	require.NoError(t, db.Exec("INSERT INTO model_configs(model,config,plugin) VALUES (?,?,?)", "test/native::text-to-video", string(pub.Config.Config), "{}").Error)
	inv := store.ReadMigrationInventory(t.Context(), store.MigrationFeatures)
	require.Equal(t, "ready", inv.State, inv.ReasonCode)
	newKey := "synthetic-probe-shadow-key"
	if len(attribution) > 0 && attribution[0] {
		newKey = "synthetic-target-key-id:synthetic-target-key-secret"
	}
	sum = sha256.Sum256([]byte(newKey))
	source := model.MigrationShadowRequest{Version: 1, OperationID: "http-probe-op", Source: model.MigrationShadowSource{SourceSnapshotHash: inv.SourceSnapshotHash, Epoch: inv.Epoch, Revision: inv.Revision, StorageGeneration: inv.StorageGeneration, From: inv.Channels[0].Identity}, Target: model.MigrationShadowTarget{CredentialID: "new-credential", CredentialDigest: hex.EncodeToString(sum[:]), Projection: model.MigrationShadowProjection{Name: "Owned shadow", Type: model.ChannelTypeFal, BaseURL: "https://queue.fal.run", Models: ch.Models, ModelMapping: ch.ModelMapping, Sets: ch.Sets, Priority: 1}}}
	if len(attribution) > 0 && attribution[0] {
		source.Target.Projection.Name = "Primary [migration]"
		value := `{"outbound_policy":"public_only"}`
		source.Target.Projection.ConfigsJSON = &value
	}
	created := store.CreateMigrationShadow(t.Context(), source, newKey, store.MigrationFeatures)
	require.Equal(t, "ready", created.State, created.Reason)
	snap, e := store.ReadMigrationSnapshot(t.Context(), created.Receipt.From, created.Receipt.Shadow)
	require.NoError(t, e)
	body := migrationProbeBody{Version: 1, Command: model.MigrationReadRequest{Seal: model.MigrationReadSealRequest{OperationID: source.OperationID, Epoch: snap.Epoch, ExpectedRevision: fmt.Sprint(snap.Revision), From: created.Receipt.From, To: created.Receipt.Shadow}, FromCredentialID: "credential", ToCredentialID: "new-credential", ExpectedPinHash: snap.PinHash, ProjectionHash: snap.ProjectionHash, FamilyHash: snap.FamilyHash}, ResourceRequestHash: created.Receipt.RequestHash, CheckpointHash: created.Receipt.CheckpointHash, Model: "test/native::text-to-video", Input: json.RawMessage(`{"model":"test/native/text-to-video","input":{"prompt":"synthetic provider only","duration":"5"}}`), GroupID: "internal-probe", TokenID: 1, MaxPrepaidMicros: "600000"}
	f := &probeHTTPFixture{db: db, store: store, body: body, initialInventory: inv, resourceRequest: source, resourceReceipt: created.Receipt}
	f.wallet = &probeHTTPWallet{t: t, db: db}
	f.engine = &nativetask.Engine{DB: db, Wallet: f.wallet}
	supplier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.providerCalls++
		var n int64
		require.NoError(t, db.Model(&model.ChannelMigrationProbeRun{}).Where("use_id<>''").Count(&n).Error)
		require.EqualValues(t, 1, n)
		if f.loseProviderReply {
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte("synthetic-upstream-id"))
	}))
	t.Cleanup(supplier.Close)
	deps := migrationProbeDependencies{billing: func(ctx context.Context, r model.MigrationProbeBillingRequest) (model.MigrationProbeBillingEvidence, error) {
		if f.billingReader == nil {
			return model.MigrationProbeBillingEvidence{}, model.ErrMigrationProbeMissingProvenance
		}
		return f.billingReader(ctx, r)
	}, store: func() (*model.ChannelAdmissionStore, error) { return store, nil }, native: nativeTrialDependencies{engine: func() (*nativetask.Engine, bool) { return f.engine, true }, channel: func(id int) (*model.Channel, error) { var c model.Channel; e := db.First(&c, id).Error; return &c, e }, provider: func(*model.Channel) nativetask.Provider { return probeHTTPProvider{supplier.URL} }}}
	f.deps = deps
	r := gin.New()
	api := r.Group("/api/channel-migrations", RequireMigrationInventoryHeaderAuth, middleware.AdminAuth)
	api.GET("/operations/:id/shadow", GetMigrationShadow)
	api.POST("/operations/:id/shadow", CreateMigrationShadow)
	api.GET("/operations/:id/shadow/lifecycle", GetMigrationShadowLifecycle)
	api.POST("/operations/:id/shadow/cancel", CancelMigrationShadow)
	api.POST("/probe-billing-context/read", ReadMigrationProbeBillingContext)
	api.POST("/operations/:id/probe", deps.create)
	api.GET("/operations/:id/probe", deps.read)
	api.POST("/operations/:id/prepare", deps.prepare)
	api.POST("/operations/:id/cutover", CutoverMigrationProbe)
	api.GET("/operations/:id", GetPreparedMigrationFacts)
	api.GET("/operations/:id/retention", GetMigrationRetention)
	api.POST("/snapshot/read", ReadMigrationSnapshotFacts)
	f.server = httptest.NewServer(r)
	t.Cleanup(f.server.Close)
	return f
}
func (f *probeHTTPFixture) call(t *testing.T, method, path string, body any, key string) (int, []byte) {
	t.Helper()
	var raw []byte
	switch b := body.(type) {
	case string:
		raw = []byte(b)
	default:
		raw, _ = json.Marshal(body)
	}
	if body == nil {
		raw = nil
	}
	req, e := http.NewRequest(method, f.server.URL+path, bytes.NewReader(raw))
	require.NoError(t, e)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, e := http.DefaultClient.Do(req)
	require.NoError(t, e)
	defer res.Body.Close()
	data, e := io.ReadAll(res.Body)
	require.NoError(t, e)
	require.Equal(t, "no-store", res.Header.Get("Cache-Control"))
	return res.StatusCode, data
}
func TestMigrationProbeHTTPActualNativeSingleSubmitAndMissingProvenance(t *testing.T) {
	f := newProbeHTTPFixture(t)
	path := "/api/channel-migrations/operations/http-probe-op/probe"
	for _, key := range []string{"", "wrong-key"} {
		status, _ := f.call(t, "POST", path, f.body, key)
		require.Equal(t, 401, status)
	}
	status, _ := f.call(t, "POST", path+"?key=ignored", f.body, config.AdminKey)
	require.Equal(t, 401, status)
	for _, bad := range []string{`{"version":1,"version":1}`, `{"passed":true}`, `null`, strings.Repeat("[", 40) + strings.Repeat("]", 40)} {
		status, _ = f.call(t, "POST", path, bad, config.AdminKey)
		require.Equal(t, 400, status)
	}
	status, raw := f.call(t, "POST", path, f.body, config.AdminKey)
	require.Equal(t, 200, status, string(raw))
	require.Contains(t, string(raw), `"taskStatus":"queued"`)
	require.NotContains(t, string(raw), "synthetic provider only")
	require.NotContains(t, string(raw), "synthetic-probe-shadow-key")
	require.NotContains(t, string(raw), "synthetic-upstream-id")
	_, raw = f.call(t, "POST", path, f.body, config.AdminKey)
	require.Contains(t, string(raw), `"taskStatus":"queued"`)
	require.Equal(t, 1, f.providerCalls)
	var response struct {
		Data model.MigrationProbeResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &response))
	obs := response.Data.Observation
	require.NotNil(t, obs)
	done, e := f.engine.Poll(t.Context(), obs.TaskID, f.body.GroupID, 1, func(_ context.Context, task *model.NativeTask) (nativetask.Poller, error) {
		require.Equal(t, "synthetic-upstream-id", task.UpstreamID)
		require.Equal(t, f.body.Command.Seal.To.KeyDigest, task.KeyFingerprint)
		return probeCompletedPoller{}, nil
	}, func(_ context.Context, id string, index int, _ string) (ownedartifact.Receipt, error) {
		return ownedartifact.Receipt{Key: fmt.Sprintf("native-results/%s/%d/%s.bin", id, index, strings.Repeat("a", 64)), SHA256: strings.Repeat("b", 64), Size: 23}, nil
	})
	require.NoError(t, e)
	require.Equal(t, "completed", done.Status)
	_, raw = f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/prepare", gin.H{"version": 1, "authorizationHash": obs.AuthorizationHash}, config.AdminKey)
	require.Contains(t, string(raw), `"reason":"missing_provenance"`)
	var count int64
	require.NoError(t, f.db.Model(&model.ChannelMigrationPrepared{}).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, f.db.Model(&model.ChannelAdmissionBarrier{}).Count(&count).Error)
	require.Zero(t, count)
	var task model.NativeTask
	require.NoError(t, f.db.First(&task, "id=?", obs.TaskID).Error)
	require.True(t, task.BillingSettled)
	require.Contains(t, task.BillingReceiptJSON, `"chargedMicros":900000`)
	// Synthetic provenance is injected only into this test invocation, never a
	// runtime environment flag or a production configuration fallback.
	reader := func(_ context.Context, request model.MigrationProbeBillingRequest) (model.MigrationProbeBillingEvidence, error) {
		return model.MigrationProbeBillingEvidence{Binding: request, EvidenceID: "local-bill", ProvenanceHash: strings.Repeat("d", 64), AccountMembershipEvidenceHash: strings.Repeat("e", 64), AccountMembershipEvidenceKind: "fal-workspace-key-membership-v1", Currency: "USD", CostMicros: 600000, CustomerDueMicros: 900000, ChargedMicros: 900000, SettledAtMs: time.Now().Add(-time.Second).UnixMilli()}, nil
	}

	// A wallet settlement without exact immutable bill provenance cannot mint a fact.
	for _, mode := range []string{"wrong-upstream", "wrong-key", "missing-account", "wrong-membership-kind", "wrong-source-key", "wrong-amount", "late-task-change"} {
		_, err := f.store.FinalizeMigrationProbe(t.Context(), f.body.Command.Seal.OperationID, func(ctx context.Context, r model.MigrationProbeBillingRequest) (model.MigrationProbeBillingEvidence, error) {
			bill, e := reader(ctx, r)
			switch mode {
			case "wrong-upstream":
				bill.Binding.UpstreamTaskID = "other-task"
			case "wrong-key":
				bill.Binding.KeyDigest = strings.Repeat("0", 64)
			case "missing-account":
				bill.AccountMembershipEvidenceHash = ""
			case "wrong-membership-kind":
				bill.AccountMembershipEvidenceKind = "permanent-account-id"
			case "wrong-source-key":
				bill.Binding.FromKeyDigest = strings.Repeat("0", 64)
			case "wrong-amount":
				bill.ChargedMicros++
			case "late-task-change":
				require.NoError(t, f.db.Model(&model.NativeTask{}).Where("id=?", obs.TaskID).UpdateColumn("upstream_id", "changed-after-reader").Error)
			}
			return bill, e
		})
		require.Error(t, err, mode)
		require.NoError(t, f.db.Model(&model.ChannelMigrationProbeRecord{}).Count(&count).Error)
		require.Zero(t, count)
		if mode == "late-task-change" {
			require.NoError(t, f.db.Model(&model.NativeTask{}).Where("id=?", obs.TaskID).UpdateColumn("upstream_id", task.UpstreamID).Error)
		}
	}
	f.billingReader = reader
	_, raw = f.call(t, "POST", "/api/channel-migrations/operations/http-probe-op/prepare", gin.H{"version": 1, "authorizationHash": obs.AuthorizationHash}, config.AdminKey)
	require.Contains(t, string(raw), `"state":"prepared"`)
	prepared, e := f.store.PrepareMigrationFromProbe(t.Context(), f.body.Command.Seal.OperationID, reader)
	require.NoError(t, e)
	require.Equal(t, "prepared", prepared.State)
	replay, e := f.store.PrepareMigrationFromProbe(t.Context(), f.body.Command.Seal.OperationID, nil)
	require.NoError(t, e)
	require.Equal(t, prepared, replay)

	// A completed task changed after proof minting cannot reuse the old proof.
	require.NoError(t, f.db.Model(&model.NativeTask{}).Where("id=?", task.ID).UpdateColumn("upstream_id", "changed-after-fact").Error)
	_, e = f.store.PrepareMigrationFromProbe(t.Context(), f.body.Command.Seal.OperationID, nil)
	require.Error(t, e)
	require.NoError(t, f.db.Model(&model.NativeTask{}).Where("id=?", task.ID).UpdateColumn("upstream_id", task.UpstreamID).Error)

	require.NoError(t, f.db.Model(&model.ChannelMigrationProbeRecord{}).Where("evidence_hash=?", prepared.ProbeHash).UpdateColumn("revoked", true).Error)
	_, e = f.store.PrepareMigrationFromProbe(t.Context(), f.body.Command.Seal.OperationID, reader)
	require.Error(t, e)
	var unchanged model.ChannelMigrationPrepared
	require.NoError(t, f.db.First(&unchanged, "operation_id=?", prepared.OperationID).Error)
	require.Equal(t, *prepared, unchanged)
	require.Equal(t, 1, f.providerCalls)
	require.NoError(t, f.db.Model(&model.ChannelAdmissionBarrier{}).Count(&count).Error)
	require.Zero(t, count)
}

type probeCompletedPoller struct{}

func (probeCompletedPoller) PollNative(context.Context, string, string, *nativeresult.CompiledTask) (nativeresult.PollResult, error) {
	return nativeresult.PollResult{Status: "result_received", Output: json.RawMessage(`{"video":{"url":"https://synthetic-provider.invalid/result.mp4"}}`)}, nil
}

func TestMigrationProbeHTTPAcceptedLostReplyNeverResubmits(t *testing.T) {
	f := newProbeHTTPFixture(t)
	f.mu.Lock()
	f.loseProviderReply = true
	f.mu.Unlock()
	path := "/api/channel-migrations/operations/http-probe-op/probe"
	_, raw := f.call(t, "POST", path, f.body, config.AdminKey)
	require.Contains(t, string(raw), `"state":"unknown"`)
	// New engine instance with only durable history, simulating process recreation.
	f.engine = &nativetask.Engine{DB: f.db, Wallet: f.wallet}
	_, raw = f.call(t, "POST", path, f.body, config.AdminKey)
	require.Contains(t, string(raw), `"state":"unknown"`)
	f.mu.Lock()
	require.Equal(t, 1, f.providerCalls)
	f.mu.Unlock()
	_, e := f.store.CancelMigrationShadow(t.Context(), f.body.Command.Seal.OperationID, f.body.ResourceRequestHash, f.body.CheckpointHash)
	require.NoError(t, e)
	_, raw = f.call(t, "POST", path, f.body, config.AdminKey)
	require.NotContains(t, string(raw), `"taskStatus":"queued"`)
	f.mu.Lock()
	require.Equal(t, 1, f.providerCalls)
	f.mu.Unlock()
	var tasks, uses int64
	require.NoError(t, f.db.Model(&model.NativeTask{}).Count(&tasks).Error)
	require.EqualValues(t, 1, tasks)
	require.NoError(t, f.db.Model(&model.ChannelAdmissionUse{}).Where("state='open'").Count(&uses).Error)
	require.EqualValues(t, 1, uses)
}
