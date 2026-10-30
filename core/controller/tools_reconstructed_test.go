// RECONSTRUCTED acceptance fixtures; no external provider or customer data.
package controller

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const recoverySigningKey = "recovery-tools-fake-signing-key-000000000000"
const recoveryProviderKey = "recovery-fake-provider-key-not-a-secret"
const recoveryModel = "local-recovery/image"

type toolsRoundTrip func(*http.Request) (*http.Response, error)

func (t toolsRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return t(r) }
func recoverySeed(t *testing.T, db *gorm.DB) model.ModelConfig {
	t.Helper()
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.ModelConfig{}, &model.GroupModelConfig{}, &model.ToolsExecution{}, &model.ToolsRequestNonce{}, &model.ToolsModelBinding{}))
	ch := model.Channel{ID: 11, Name: "reconstructed-synthetic-only", Type: model.ChannelTypeFal, Status: model.ChannelStatusEnabled, Key: recoveryProviderKey}
	require.NoError(t, db.FirstOrCreate(&ch, model.Channel{ID: 11}).Error)
	contract := json.RawMessage(`{"version":1,"model":"local-recovery/image","input_schema":{"type":"object","required":["prompt"],"additionalProperties":false,"properties":{"prompt":{"type":"string","minLength":1,"maxLength":6000},"image_urls":{"type":"array","maxItems":4,"items":{"type":"string","pattern":"^data:image/png;base64,[A-Za-z0-9+/=]+$"}},"image_size":{"default":"square_hd","anyOf":[{"enum":["square_hd"]},{"type":"object","required":["width","height"],"additionalProperties":false,"properties":{"width":{"type":"integer","minimum":64,"maximum":8192},"height":{"type":"integer","minimum":64,"maximum":8192}}}]}}},"output_schema":{"type":"object","required":["images"],"properties":{"images":{"type":"array","minItems":1,"maxItems":1,"items":{"type":"object","required":["url"],"properties":{"url":{"type":"string"}}}}}},"artifacts":[{"path":["images","*","url"]}]}`)
	binding := nativeRouteBinding{Version: 1, ChannelID: 11, Endpoint: "fal-ai/recovery-fixture", CredentialScope: "synthetic-only", KeyFingerprint: model.ImageChannelKeyFingerprint(ch.Key), DeliveryBase: "https://agent-gateway.invalid", Contract: contract}
	raw, _ := json.Marshal(binding)
	row := model.ToolsModelBinding{Model: recoveryModel, Capability: "image-to-image", Enabled: true, BindingJSON: string(raw), SourceJSON: `{"modelId":"synthetic","capabilityId":"synthetic","draftId":"synthetic","draftVersion":1,"channelId":"11"}`, Revision: toolsDigest(raw)}
	require.NoError(t, db.FirstOrCreate(&row, model.ToolsModelBinding{Model: recoveryModel}).Error)
	mc, ok := toolsBindingConfig(row)
	require.True(t, ok)
	return mc
}
func toolsRoutes(r *gin.Engine, d toolsDependencies) {
	group := r.Group("/internal/tools", ToolsAuth)
	group.GET("/models", GetToolsModelOptions)
	group.GET("/model-access", GetToolsModelAccess)
	group.POST("/model-tasks", d.create)
	group.GET("/model-tasks/:id", func(c *gin.Context) { d.read(c, false) })
	group.POST("/model-tasks/:id/cancel", func(c *gin.Context) { d.read(c, true) })
	group.GET("/model-tasks/:id/artifacts/:index", d.artifact)
}
func toolsControllerSetup(t *testing.T) (*gin.Engine, *gorm.DB, model.ModelConfig) {
	t.Helper()
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "gateway.db"))
	require.NoError(t, err)
	previous, previousLog := model.DB, model.LogDB
	model.DB, model.LogDB = db, db
	t.Cleanup(func() { model.DB, model.LogDB = previous, previousLog; sql, _ := db.DB(); sql.Close() })
	t.Setenv("TOOLS_INTERNAL_KEY", recoverySigningKey)
	common.UsingSQLite = true
	mc := recoverySeed(t, db)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	toolsRoutes(r, defaultToolsDependencies())
	return r, db, mc
}
func signedToolsRequest(t *testing.T, method, path, body, id, revision, user string) *http.Request {
	t.Helper()
	var nonce [16]byte
	_, err := rand.Read(nonce[:])
	require.NoError(t, err)
	claims := toolsClaims{V: 1, UserID: user, AgentID: "recovery-agent", SessionID: "recovery-session", RequestID: id, Method: method, Path: path, BodyHash: toolsDigest([]byte(body)), Revision: revision, IAT: time.Now().Unix(), Nonce: hex.EncodeToString(nonce[:])}
	raw, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(recoverySigningKey))
	mac.Write([]byte("musespan-tools-execution-v1." + payload))
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Tools-Authorization", payload+"."+base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Request-Id", id)
	if revision != "" {
		req.Header.Set("X-Tools-Execution-Revision", revision)
	}
	return req
}
func toolsSend(r http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
func toolsRevisionFor(t *testing.T, r http.Handler) string {
	w := toolsSend(r, signedToolsRequest(t, "GET", "/internal/tools/model-access?model="+recoveryModel, "", "access", "", "owner"))
	require.Equal(t, 200, w.Code, w.Body.String())
	var v map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v))
	return v["revision"].(string)
}
func TestReconstructedToolsAuthLimits(t *testing.T) {
	r, _, _ := toolsControllerSetup(t)
	calls := 0
	r.POST("/internal/tools/limit", ToolsAuth, func(c *gin.Context) { calls++; c.Status(204) })
	for _, size := range []int{2 << 20, 4 << 20, (4 << 20) + 1} {
		req := signedToolsRequest(t, "POST", "/internal/tools/limit", strings.Repeat("x", size), "limit", "", "owner")
		w := toolsSend(r, req)
		expected := 204
		if size > 4<<20 {
			expected = 413
		}
		require.Equal(t, expected, w.Code)
	}
	require.Equal(t, 2, calls)
	req := signedToolsRequest(t, "GET", "/internal/tools/models", "", "nonce", "", "owner")
	header := req.Header.Clone()
	require.Equal(t, 200, toolsSend(r, req).Code)
	second := httptest.NewRequest("GET", "/internal/tools/models", nil)
	second.Header = header
	require.Equal(t, 401, toolsSend(r, second).Code)
}

type recoveryProvider struct {
	mu               sync.Mutex
	submits, cancels int
	unknown          bool
}

func (p *recoveryProvider) SubmitNative(_ context.Context, _ string, _ []byte) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.submits++
	if p.unknown {
		return "", errors.New("unknown acceptance")
	}
	return "synthetic", nil
}
func (p *recoveryProvider) PollNative(context.Context, string, string, *nativeresult.CompiledTask) (nativeresult.PollResult, error) {
	return nativeresult.PollResult{Status: "running"}, nil
}
func (p *recoveryProvider) CancelNative(context.Context, string, string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancels++
	return "cancellation_requested", nil
}
func TestReconstructedToolsSubmissionBoundaryAndNoRetry(t *testing.T) {
	_, db, mc := toolsControllerSetup(t)
	provider := &recoveryProvider{unknown: true}
	deps := defaultToolsDependencies()
	deps.provider = func(*model.Channel) toolsProvider { return provider }
	r := gin.New()
	toolsRoutes(r, deps)
	revision := toolsRevisionFor(t, r)
	body := `{"model":"local-recovery/image","input":{"prompt":"test"}}`
	longest := "tools_" + strings.Repeat("x", 180)
	for i := 0; i < 2; i++ {
		w := toolsSend(r, signedToolsRequest(t, "POST", "/internal/tools/model-tasks", body, longest, revision, "owner"))
		require.Contains(t, []int{200, 202}, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "unconfirmed")
	}
	require.Equal(t, 1, provider.submits)
	bad := toolsSend(r, signedToolsRequest(t, "POST", "/internal/tools/model-tasks", body, longest+"x", revision, "owner"))
	require.Equal(t, 400, bad.Code)
	conflict := toolsSend(r, signedToolsRequest(t, "POST", "/internal/tools/model-tasks", body+" ", longest, revision, "owner"))
	require.Equal(t, 409, conflict.Code)
	foreign := toolsSend(r, signedToolsRequest(t, "GET", "/internal/tools/model-tasks/"+longest, "", "foreign", "", "other"))
	require.Equal(t, 404, foreign.Code)
	require.NoError(t, db.Model(&model.ToolsModelBinding{}).Where("model = ?", mc.Model).Update("enabled", false).Error)
	replay := toolsSend(r, signedToolsRequest(t, "POST", "/internal/tools/model-tasks", body, longest, revision, "owner"))
	require.Contains(t, []int{200, 202}, replay.Code)
	require.Equal(t, 1, provider.submits)
}
func TestReconstructedToolsParallelSubmission(t *testing.T) {
	_, _, _ = toolsControllerSetup(t)
	p := &recoveryProvider{}
	d := defaultToolsDependencies()
	d.provider = func(*model.Channel) toolsProvider { return p }
	r := gin.New()
	toolsRoutes(r, d)
	rev := toolsRevisionFor(t, r)
	body := `{"model":"local-recovery/image","input":{"prompt":"test"}}`
	requests := make([]*http.Request, 8)
	for i := range requests {
		requests[i] = signedToolsRequest(t, "POST", "/internal/tools/model-tasks", body, "tools_same", rev, "owner")
	}
	var wg sync.WaitGroup
	for _, req := range requests {
		wg.Add(1)
		go func(req *http.Request) { defer wg.Done(); toolsSend(r, req) }(req)
	}
	wg.Wait()
	require.Equal(t, 1, p.submits)
	for i := 0; i < 2; i++ {
		w := toolsSend(r, signedToolsRequest(t, "POST", "/internal/tools/model-tasks/tools_same/cancel", "", fmt.Sprint("cancel", i), "", "owner"))
		require.Equal(t, 200, w.Code)
	}
	require.Equal(t, 1, p.cancels)
}

// Loopback redirection exists only in this opt-in test binary. Production Fal
// origin and archive URL/DNS policies remain unchanged.
type recoveryLoopbackTransport struct{ real http.RoundTripper }

func (t recoveryLoopbackTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	req := r.Clone(r.Context())
	target := *r.URL
	req.URL = &target
	switch target.Hostname() {
	case "queue.fal.run", "agent-fixture.invalid":
		req.URL.Scheme = "http"
		req.URL.Host = "127.0.0.1:13512"
	default:
		return nil, errors.New("recovery outbound blocked")
	}
	return t.real.RoundTrip(req)
}
func TestReconstructedToolsLoopbackHarness(t *testing.T) {
	if os.Getenv("TOOLS_RECOVERY_HARNESS") != "true" {
		t.Skip("opt-in isolated harness")
	}
	root := os.Getenv("TOOLS_RECOVERY_STATE")
	require.True(t, filepath.IsAbs(root))
	require.Contains(t, root, "recovery")
	require.NoError(t, os.MkdirAll(root, 0700))
	db, err := model.OpenSQLite(filepath.Join(root, "gateway-reconstructed.db"))
	require.NoError(t, err)
	model.DB, model.LogDB = db, db
	common.UsingSQLite = true
	common.RedisEnabled = false
	t.Setenv("TOOLS_INTERNAL_KEY", recoverySigningKey)
	t.Setenv("EXTERNAL_BALANCE_URL", "http://127.0.0.1:13510")
	t.Setenv("EXTERNAL_BALANCE_KEY", "recovery-fake-storage-key-not-a-secret")
	recoverySeed(t, db)
	transport := recoveryLoopbackTransport{real: &http.Transport{Proxy: nil}}
	client := &http.Client{Transport: transport, Timeout: 45 * time.Second}
	deps := defaultToolsDependencies()
	deps.provider = func(ch *model.Channel) toolsProvider { return &fal.Client{HTTP: client, Key: ch.Key} }
	deps.archive = func(ctx context.Context, task string, index int, source string) (ownedartifact.Receipt, error) {
		return ownedartifact.StoreWithSourceClient(ctx, task, index, source, client)
	}
	r := gin.New()
	r.Use(gin.Recovery())
	toolsRoutes(r, deps)
	t.Log("RECONSTRUCTED synthetic Tools harness ready at 127.0.0.1:13511; model=" + recoveryModel)
	require.NoError(t, http.ListenAndServe("127.0.0.1:13511", r))
}

func TestReconstructedSavedWanContracts(t *testing.T) {
	_, _, legacy := toolsControllerSetup(t)
	binding, ok := parseNativeRouteBinding(legacy)
	require.True(t, ok)
	var accesses []struct {
		Model    string          `json:"model"`
		Contract json.RawMessage `json:"contract"`
		Agent    map[string]any  `json:"agent"`
	}
	raw, err := os.ReadFile("testdata/wan-edit-tools/saved-accesses.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &accesses))
	require.Len(t, accesses, 2)
	for _, access := range accesses {
		t.Run(access.Model, func(t *testing.T) {
			b := binding
			b.Contract = access.Contract
			b.Endpoint = access.Agent["upstreamModelId"].(string)
			encoded, _ := json.Marshal(b)
			capability := access.Agent["capability"].(string)
			row := model.ToolsModelBinding{Model: access.Model, Capability: capability, Enabled: true, BindingJSON: string(encoded)}
			mc, ok := toolsBindingConfig(row)
			require.True(t, ok)
			mapping, _, _, ok := toolsDescriptor(mc)
			require.True(t, ok)
			require.Equal(t, access.Agent["promptField"], mapping["promptField"])
			require.Equal(t, access.Agent["referenceField"], mapping["referenceField"])
			require.Equal(t, access.Agent["referenceFormat"], mapping["referenceFormat"])
			expected := access.Agent["parameters"].(map[string]any)
			actual := mapping["parameters"].(map[string]string)
			for key, value := range expected {
				require.Equal(t, value, actual[key], key)
			}
		})
	}
}
