package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// Real application transport/orchestration/store against real private handlers
// and wallet. Only source/price/identity lookup and provider generation are fixtures.
func TestNativePrivateTrialApplicationFlow(t *testing.T) {
	for _, image := range []bool{false, true} {
		for _, actualCost := range []bool{false, true} {
			t.Run(fmt.Sprintf("image-priced-%t-actual-cost-%t", image, actualCost), func(t *testing.T) { runNativePrivateApplicationFlow(t, image, actualCost, false) })
		}
	}
	for _, image := range []bool{false, true} {
		t.Run(fmt.Sprintf("historical-image-%t", image), func(t *testing.T) { runNativePrivateApplicationFlow(t, image, true, true) })
	}
	for _, image := range []bool{false, true} {
		t.Run(fmt.Sprintf("platform-image-%t", image), func(t *testing.T) { runNativePrivateApplicationFlow(t, image, true, false, true) })
	}

}

type applicationTrialProvider struct {
	trialProvider
	upstreamID string
}

func (p *applicationTrialProvider) SubmitNative(ctx context.Context, endpoint string, input []byte) (string, error) {
	_, err := p.trialProvider.SubmitNative(ctx, endpoint, input)
	return p.upstreamID, err
}
func runNativePrivateApplicationFlow(t *testing.T, image, actualCost, historical bool, platform ...bool) {
	platformReserve := len(platform) > 0 && platform[0]
	appRoot, walletURL := os.Getenv("NATIVE_TEST_APP_ROOT"), os.Getenv("D34_TEST_WALLET_URL")
	if appRoot == "" || walletURL == "" {
		t.Skip("requires disposable application runner")
	}
	require.True(t, strings.HasPrefix(walletURL, "http://127.0.0.1:"))
	database, err := model.OpenSQLite(filepath.Join(t.TempDir(), "native-private.db"))
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.NativeTask{}))
	conn, err := database.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	engine := &nativetask.Engine{DB: database, Wallet: balance.NewExternalHTTP(walletURL, "local-d34-test-only")}
	provider := &applicationTrialProvider{upstreamID: fmt.Sprintf("private-upstream-%t-%t-%t", image, actualCost, historical)}
	channel := &model.Channel{ID: 9, Type: model.ChannelTypeFal, Status: model.ChannelStatusDisabled, Key: "trial-secret", BaseURL: "https://queue.fal.run"}
	deps := nativeTrialDependencies{
		engine: func() (*nativetask.Engine, bool) { return engine, true },
		token: func(id int) (*model.Token, error) {
			return &model.Token{ID: id, GroupID: "u_u", Status: model.TokenStatusEnabled}, nil
		},
		group: func(string, bool) (*model.Group, error) {
			return &model.Group{ID: "u_u", Status: model.GroupStatusInternal}, nil
		},
		channel:  func(int) (*model.Channel, error) { return channel, nil },
		provider: func(*model.Channel) nativetask.Provider { return provider },
	}
	oldKey := config.AdminKey
	config.AdminKey = "private-admin-test"
	t.Cleanup(func() { config.AdminKey = oldKey })
	router := gin.New()
	api := router.Group("/api", middleware.AdminAuth)
	api.POST("/native-trials/:group/:token", deps.create)
	api.GET("/native-trials/:group/:token/:id", func(c *gin.Context) { deps.read(c, false) })
	// Test-only completion advances the synthetic provider result through actual
	// delivery/wallet services. No poller or provider network is invoked.
	api.POST("/_test/complete/:id", func(c *gin.Context) {
		id := c.Param("id")
		result := []byte(`{"text":"isolated native result","seed":9007199254740993}`)
		var archive nativetask.ArchiveFunc
		if image {
			result = []byte(`{"image":{"url":"https://provider.example/vector.svg"},"seed":9007199254740993}`)
			if actualCost {
				result = []byte(`{"images":[{"url":"https://provider.example/vector.svg"}],"seed":9007199254740993}`)
			}
			base := os.Getenv("NATIVE_ARTIFACT_TEST_URL")
			if !strings.HasPrefix(base, "http://127.0.0.1:") {
				c.Status(500)
				return
			}
			archive = func(ctx context.Context, task string, index int, source string) (ownedartifact.Receipt, error) {
				var empty ownedartifact.Receipt
				if source != "https://provider.example/vector.svg" {
					return empty, fmt.Errorf("unexpected test source")
				}
				req, err := http.NewRequestWithContext(ctx, "POST", base+"/api/internal/media/files", bytes.NewBufferString("<svg>isolated</svg>"))
				if err != nil {
					return empty, err
				}
				req.Header.Set("Authorization", "Bearer local-native-test-only")
				req.Header.Set("Content-Type", "application/octet-stream")
				req.Header.Set("X-Native-Task-ID", task)
				req.Header.Set("X-Native-Artifact-Index", fmt.Sprint(index))
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					return empty, err
				}
				defer response.Body.Close()
				var stored struct {
					Code int                   `json:"code"`
					Data ownedartifact.Receipt `json:"data"`
				}
				if err := json.NewDecoder(response.Body).Decode(&stored); err != nil {
					return empty, err
				}
				if response.StatusCode != 200 || stored.Code != 0 || !ownedartifact.ValidReceipt(task, index, stored.Data) {
					return empty, fmt.Errorf("invalid isolated archive receipt")
				}
				return stored.Data, nil
			}
		}
		if err := model.SaveNativeTaskResult(database, id, "u_u", 7, result); err != nil {
			c.Status(500)
			return
		}
		if _, err := engine.Deliver(c.Request.Context(), id, "u_u", 7, archive); err != nil {
			c.Status(500)
			return
		}
		c.JSON(200, gin.H{"success": true, "data": true})
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--import", "tsx", "tests/fixtures/native-admin-trial-client.ts")
	cmd.Dir = appRoot
	cmd.Env = append(os.Environ(), "AIPROXY_BASE_URL="+server.URL, "AIPROXY_ADMIN_KEY=private-admin-test", "NATIVE_PRIVATE_TEST_URL="+server.URL, fmt.Sprintf("NATIVE_PRIVATE_TEST_IMAGE=%t", image), fmt.Sprintf("NATIVE_PRIVATE_TEST_ACTUAL_COST=%t", actualCost), fmt.Sprintf("NATIVE_PRIVATE_TEST_HISTORICAL=%t", historical), "NATIVE_PRIVATE_TEST_UPSTREAM_ID="+provider.upstreamID, fmt.Sprintf("NATIVE_PRIVATE_TEST_PLATFORM=%t", platformReserve))
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	t.Log(string(output))
	require.Equal(t, 1, provider.calls)
	require.Contains(t, provider.input, "9007199254740993")
	require.False(t, database.Migrator().HasTable(&model.ModelConfig{}))
}
