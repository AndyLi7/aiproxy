//nolint:testpackage
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor/openai"
	relaycontroller "github.com/labring/aiproxy/core/relay/controller"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type relayAdmissionHarness struct {
	db             *gorm.DB
	store          *model.ChannelAdmissionStore
	proof          model.VerifiedDeploymentProof
	channels       []*model.Channel
	calls          atomic.Int32
	missingUse     atomic.Bool
	responseStatus int
	stream         bool
}

func newRelayAdmissionHarness(t *testing.T) *relayAdmissionHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := &relayAdmissionHarness{responseStatus: http.StatusOK}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.calls.Add(1)
		var count int64
		if err := h.db.Model(&model.ChannelAdmissionUse{}).Where("state = ? AND lane = ?", "open", "relay").Count(&count).Error; err != nil || count == 0 {
			h.missingUse.Store(true)
		}
		if h.stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(h.responseStatus)
			_, _ = w.Write([]byte("data: {\"id\":\"synthetic\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"index\":0}]}\n\ndata: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(h.responseStatus)
		_, _ = w.Write([]byte(`{"id":"synthetic","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`))
	}))
	t.Cleanup(server.Close)
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "relay.db"))
	require.NoError(t, err)
	h.db = db
	pool, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.ChannelAdmissionControl{}, &model.ChannelAdmissionProof{}, &model.ChannelAdmissionFamily{}, &model.ChannelAdmissionBarrier{}, &model.ChannelAdmissionUse{}))
	executor := model.ChannelExecutorIdentity{ServiceID: "synthetic-relay", InstanceID: "synthetic-instance", BuildSHA: strings.Repeat("a", 64), Epoch: "synthetic-epoch"}
	h.proof = model.VerifiedDeploymentProof{ProofID: "synthetic-proof", Epoch: executor.Epoch, Sequence: 1, StorageGeneration: "synthetic-storage", ExecutorFenceHash: strings.Repeat("b", 64), IssuedAtMs: time.Now().Add(-time.Minute).UnixMilli(), ValidUntilMs: time.Now().Add(time.Minute).UnixMilli(), RevocationFreshUntilMs: time.Now().Add(time.Minute).UnixMilli(), Executors: []model.ChannelExecutorIdentity{executor}}
	encoded, err := json.Marshal(h.proof.Executors)
	require.NoError(t, err)
	hash := sha256.Sum256(encoded)
	h.proof.MembershipHash = hex.EncodeToString(hash[:])
	proofHash, err := h.proof.Fingerprint()
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.ChannelAdmissionControl{ID: 1, Version: 1, Epoch: h.proof.Epoch, EpochSequence: 1, StorageGeneration: h.proof.StorageGeneration, MembershipHash: h.proof.MembershipHash, ExecutorFenceHash: h.proof.ExecutorFenceHash, ActiveProofID: h.proof.ProofID}).Error)
	require.NoError(t, db.Create(&model.ChannelAdmissionProof{ID: h.proof.ProofID, FactHash: proofHash, ConsumedSequence: 1}).Error)
	h.store = &model.ChannelAdmissionStore{DB: db, LogDB: db, Executor: executor, LoadDeployment: func(context.Context) (model.VerifiedDeploymentProof, error) { return h.proof, nil }}
	for i := 1; i <= 2; i++ {
		channel := &model.Channel{ID: i, Type: model.ChannelTypeOpenAI, Key: "synthetic-shared-key", BaseURL: server.URL, Status: model.ChannelStatusEnabled}
		require.NoError(t, db.Create(channel).Error)
		h.channels = append(h.channels, channel)
	}
	oldDB, oldLog := model.DB, model.LogDB
	model.DB, model.LogDB = db, db
	restore := model.ConfigureChannelAdmissionRuntime(executor, h.store.LoadDeployment)
	t.Cleanup(func() { restore(); model.DB, model.LogDB = oldDB, oldLog })
	return h
}

func (h *relayAdmissionHarness) seal(t *testing.T) {
	t.Helper()
	from, err := model.ChannelIdentityOf(h.channels[0])
	require.NoError(t, err)
	to, err := model.ChannelIdentityOf(h.channels[1])
	require.NoError(t, err)
	_, err = h.store.Seal(context.Background(), model.ChannelSealRequest{OperationID: "synthetic-move", Epoch: h.proof.Epoch, ExpectedRevision: 0, From: from, To: to})
	require.NoError(t, err)
}
func (h *relayAdmissionHarness) call(t *testing.T, index int, nested, internal bool, m mode.Mode) (*relaycontroller.HandleResult, *meta.Meta) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"synthetic","messages":[{"role":"user","content":"hi"}],"stream":`+map[bool]string{true: "true", false: "false"}[h.stream]+`}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("X-Request-ID", "reused-client-request")
	c.Request.Header.Set("X-Channel-Admission-Use", "forged-use")
	c.Request.Header.Set("X-Channel-Migration-Probe", "true")
	mt := meta.NewMeta(h.channels[index], m, "synthetic", model.ModelConfig{}, meta.WithRequestID("reused-client-request"), meta.WithBillingOperationID("server-billing-id"))
	if !internal {
		mt.Group = model.GroupCache{ID: "group"}
		mt.Token = model.TokenCache{ID: 1, Group: "group"}
	}
	// A forged/reused metadata reference is not a grant either.
	mt.ChannelAdmissionUseID = "forged-use"
	if nested {
		return relaycontroller.Handle(&openai.Adaptor{}, c, mt, AdaptorStore), mt
	}
	return relayHandler(c, mt, &model.ModelCaches{}), mt
}

func TestRelayAdmissionActualHandlerPersistsBeforeTransportAndRetainsUnknown(t *testing.T) {
	for _, scenario := range []string{"public", "stream", "internal-admin", "nested-plugin", "ambiguous-response"} {
		t.Run(scenario, func(t *testing.T) {
			h := newRelayAdmissionHarness(t)
			h.stream = scenario == "stream"
			if scenario == "ambiguous-response" {
				h.responseStatus = http.StatusServiceUnavailable
			}
			result, mt := h.call(t, 0, scenario == "nested-plugin", scenario == "internal-admin", mode.ChatCompletions)
			if scenario == "ambiguous-response" {
				require.NotNil(t, result.Error)
			} else {
				require.Nil(t, result.Error)
			}
			require.Equal(t, int32(1), h.calls.Load())
			require.False(t, h.missingUse.Load())
			require.Len(t, mt.ChannelAdmissionUseID, 64)
			var use model.ChannelAdmissionUse
			require.NoError(t, h.db.First(&use, "id = ?", mt.ChannelAdmissionUseID).Error)
			require.Equal(t, "open", use.State)
			require.Zero(t, use.ClosedAtMs)
			// No async outbox or task table exists in this fixture: admission must
			// already survive the provider-accepted/before-persistence crash window.
			h.seal(t)
			retry, _ := h.call(t, 0, false, false, mode.ChatCompletions)
			require.NotNil(t, retry.Error)
			require.Equal(t, int32(1), h.calls.Load())
			require.NoError(t, h.db.First(&use, "id = ?", mt.ChannelAdmissionUseID).Error)
			require.Equal(t, "open", use.State)
		})
	}
}

func TestRelayAdmissionActualHandlerRejectsWithoutSending(t *testing.T) {
	for _, scenario := range []string{"sealed-public", "sealed-admin", "sealed-nested", "stale-key", "stale-host", "revoked", "partial", "split", "disabled-shadow", "preview"} {
		t.Run(scenario, func(t *testing.T) {
			h := newRelayAdmissionHarness(t)
			switch scenario {
			case "sealed-public", "sealed-admin", "sealed-nested":
				h.seal(t)
			case "stale-key":
				h.channels[0].Key = "changed-synthetic-key"
			case "stale-host":
				h.channels[0].BaseURL = "https://must-not-send.invalid"
			case "revoked":
				require.NoError(t, h.db.Model(&model.ChannelAdmissionProof{}).Where("id = ?", h.proof.ProofID).Update("revoked", true).Error)
			case "partial":
				require.NoError(t, h.db.Migrator().DropTable(&model.ChannelAdmissionUse{}))
			case "split":
				model.LogDB = h.db.Session(&gorm.Session{})
			case "disabled-shadow":
				require.NoError(t, h.db.Model(&model.Channel{}).Where("id = ?", 1).Update("status", model.ChannelStatusDisabled).Error)
			case "preview":
				h.channels[0].ID = 0
			}
			result, mt := h.call(t, 0, scenario == "sealed-nested", scenario == "sealed-admin", mode.ChatCompletions)
			require.NotNil(t, result.Error)
			require.Contains(t, result.Error.Error(), "channel admission unavailable")
			require.Empty(t, mt.ChannelAdmissionUseID)
			require.Zero(t, h.calls.Load())
		})
	}
}

func TestRelayAdmissionReadsDoNotCreateNewUseAndSameKeySuccessorWorks(t *testing.T) {
	h := newRelayAdmissionHarness(t)
	h.seal(t)
	// Reuse actual DoHelper/Handle with a read mode. An invalid read request
	// may fail in the adaptor; it must not fail the new-work admission gate.
	result, mt := h.call(t, 0, true, false, mode.ResponsesGet)
	if result.Error != nil {
		require.NotContains(t, result.Error.Error(), "channel admission unavailable")
	}
	require.Empty(t, mt.ChannelAdmissionUseID)
	var count int64
	require.NoError(t, h.db.Model(&model.ChannelAdmissionUse{}).Count(&count).Error)
	require.Zero(t, count)
	before := h.calls.Load()
	result, mt = h.call(t, 1, false, false, mode.ChatCompletions)
	require.Nil(t, result.Error)
	require.Equal(t, before+1, h.calls.Load())
	require.NotEmpty(t, mt.ChannelAdmissionUseID)
}
