//nolint:testpackage // Loopback integration uses the production parser, native engine and image renderer.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/imagecompat"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

type imageSDKWallet struct {
	mu       sync.Mutex
	claimed  map[string]bool
	commands []balance.PrepaymentCommand
}

func (w *imageSDKWallet) Prepayment(
	_ context.Context,
	c balance.PrepaymentCommand,
) (balance.PrepaymentReceipt, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.commands = append(w.commands, c)

	result := balance.PrepaymentReceipt{ID: c.BillingOperationID}
	if c.Action == "begin_attempt" {
		result.Claimed = !w.claimed[c.BillingOperationID]
		w.claimed[c.BillingOperationID] = true
	}

	return result, nil
}

type imageSDKProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *imageSDKProvider) SubmitNative(context.Context, string, []byte) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls++

	return fmt.Sprintf("synthetic-%d", p.calls), nil
}

func imageSDKPlan(t *testing.T, id string) nativetask.Plan {
	t.Helper()

	properties := map[string]any{
		"prompt":        map[string]any{"type": "string", "minLength": 2, "maxLength": 32000},
		"num_images":    map[string]any{"const": 1},
		"image_size":    map[string]any{"enum": []string{"1024x1024", "1536x1024", "1024x1536"}},
		"quality":       map[string]any{"enum": []string{"low", "medium", "high"}},
		"background":    map[string]any{"enum": []string{"auto", "opaque", "transparent"}},
		"output_format": map[string]any{"enum": []string{"png", "jpeg", "webp"}},
	}

	required := []string{"prompt"}
	if id == imagecompat.EditModel {
		properties["image_urls"] = map[string]any{
			"type":     "array",
			"minItems": 1,
			"maxItems": 1,
			"items":    map[string]any{"type": "string"},
		}
		properties["image_size"] = map[string]any{
			"enum": []string{"auto", "1024x1024", "1536x1024", "1024x1536"},
		}
		properties["input_fidelity"] = map[string]any{"enum": []string{"low", "high"}}

		required = append(required, "image_urls")
	}

	contract := nativeresult.TaskContract{
		Version:   1,
		Model:     id,
		Artifacts: []nativeresult.ArtifactBinding{{Path: []string{"images", "*", "url"}}},
	}
	inputSchema, err := json.Marshal(
		map[string]any{
			"type":                 "object",
			"properties":           properties,
			"required":             required,
			"additionalProperties": false,
		},
	)
	require.NoError(t, err)

	contract.InputSchema = inputSchema
	contract.OutputSchema = json.RawMessage(
		`{"type":"object","required":["images"],"properties":{"images":{"type":"array","items":{"type":"object","required":["url"],"properties":{"url":{"type":"string"}},"additionalProperties":false}}},"additionalProperties":false}`,
	)
	raw, err := json.Marshal(contract)
	require.NoError(t, err)

	return nativetask.Plan{
		Contract:        raw,
		ChannelID:       1,
		Endpoint:        "fal-ai/synthetic-only",
		CredentialScope: "fixture",
		KeyFingerprint:  model.ImageChannelKeyFingerprint("synthetic-key"),
		DeliveryBase:    "https://owned.invalid",
		QuoteJSON:       `{"version":1,"currency":"USD","quoteVersion":"synthetic","prepaidMicros":200,"routes":[{"routeId":"r","channelId":1,"provider":"fal","endpoint":"fal-ai/synthetic-only","credentialScope":"fixture","quantityMetric":"bounded_request","estimatedMicros":100,"prepaidMicros":200,"rule":{"mode":"cost_markup","ratio":"1"}}]}`,
	}
}

func TestImageCompatOfficialSDKs(t *testing.T) {
	python, nodeSDK := os.Getenv("AND76_PYTHON"), os.Getenv("AND76_NODE_SDK")
	if python == "" || nodeSDK == "" {
		t.Skip("set isolated official SDK paths to run loopback integration")
	}

	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "sdk.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))

	oldDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = oldDB })

	wallet := &imageSDKWallet{claimed: map[string]bool{}}
	provider := &imageSDKProvider{}
	engine := &nativetask.Engine{DB: db, Wallet: wallet, Provider: provider}

	var pngBuffer bytes.Buffer
	require.NoError(t, png.Encode(&pngBuffer, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	data := pngBuffer.Bytes()
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	//nolint:unparam // Transport signature is required by the native HTTP service.
	download := func(*http.Request, string) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: int64(len(data)),
			Body:          io.NopCloser(bytes.NewReader(data)),
		}, nil
	}
	//nolint:unparam // Archive callback signature is required by the native executor.
	archive := func(_ context.Context, id string, index int, _ string) (ownedartifact.Receipt, error) {
		return ownedartifact.Receipt{
			Key:    fmt.Sprintf("native-results/%s/%d/%s.bin", id, index, digest),
			Size:   int64(len(data)),
			SHA256: digest,
			Type:   "image/png",
		}, nil
	}

	t.Setenv("EXTERNAL_BALANCE_KEY", strings.Repeat("fixture-secret-", 4))

	router := gin.New()
	router.GET(
		"/v1/images/native-results/:id/:index",
		func(c *gin.Context) { getNativeImageContent(c, &nativetask.HTTP{Download: download}) },
	)

	calls := map[string]int{}

	var callsMu sync.Mutex

	handler := func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer fake-image-sdk-key" {
			c.AbortWithStatus(401)
			return
		}

		c.Set(middleware.Group, model.GroupCache{ID: "g"})
		c.Set(middleware.Token, model.TokenCache{ID: 1})
		c.Header("x-should-retry", "false")

		id, err := imagecompat.OperationID(c.Request.Header, "g", 1)
		if err != nil {
			imageCompatError(c, 400, "invalid_idempotency", err.Error(), "")
			return
		}

		parsed, err := imagecompat.Parse(c.Request, strings.HasSuffix(c.Request.URL.Path, "/edits"))
		if err != nil {
			imageCompatError(c, 400, "invalid_input", "Unsupported input", "")
			return
		}

		if parsed.Model != imagecompat.GenerateModel && parsed.Model != imagecompat.EditModel {
			imageCompatError(c, 400, "invalid_model", "Invalid model", "")
			return
		}

		var native struct {
			Input struct {
				Prompt string `json:"prompt"`
			} `json:"input"`
		}
		require.NoError(t, json.Unmarshal(parsed.Body, &native))
		prompt := native.Input.Prompt

		callsMu.Lock()
		counterKey := prompt + "|" + c.GetHeader("X-Stainless-Lang")
		calls[counterKey]++
		attempt := calls[counterKey]
		callsMu.Unlock()

		plan := imageSDKPlan(t, parsed.Model)
		h := &nativetask.HTTP{
			Engine:      engine,
			Identity:    func(*http.Request) (string, int, error) { return "g", 1, nil },
			Download:    download,
			ResolvePlan: func(*http.Request, []byte) (nativetask.Plan, nativetask.Provider, error) { return plan, provider, nil },
		}

		task := h.CreateTask(c.Writer, c.Request, id, parsed.Body)
		if task == nil {
			return
		}

		if prompt != "pending" && task.Status != "completed" {
			require.NoError(
				t,
				model.SaveNativeTaskResult(
					db,
					id,
					"g",
					1,
					[]byte(`{"images":[{"url":"https://provider.invalid/image.png"}]}`),
				),
			)
			_, err := engine.Deliver(context.Background(), id, "g", 1, archive)
			require.NoError(t, err)
		}

		if strings.HasPrefix(prompt, "slow-") && attempt%2 == 1 {
			time.Sleep(150 * time.Millisecond)
		}

		serveImageCompat(
			c,
			&imageCompatCall{request: parsed, id: id, until: time.Now().Add(5 * time.Millisecond)},
			h,
		)
	}
	router.POST("/v1/images/generations", handler)
	router.POST("/v1/images/edits", handler)

	server := httptest.NewServer(router)
	defer server.Close()

	t.Setenv("PUBLIC_IMAGE_BASE_URL", server.URL)

	for _, spec := range []struct{ binary, script string }{{"node", "node.mjs"}, {python, "python.py"}} {
		script := filepath.Join("testdata", "imagecompat", spec.script)
		// #nosec G204 -- Local test operator supplies SDK runtimes; no request controls executable paths.
		cmd := exec.CommandContext(t.Context(), spec.binary, script)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"AND76_TEST_BASE=" + server.URL + "/v1",
			"AND76_NODE_SDK=" + nodeSDK,
			"AND76_TEST_PNG=" + base64.StdEncoding.EncodeToString(data),
		}
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, string(output))
		t.Log(strings.TrimSpace(string(output)))
	}

	provider.mu.Lock()
	require.Equal(
		t,
		10,
		provider.calls,
		"5 independent generations per SDK; retries and format changes do not dispatch",
	)
	provider.mu.Unlock()
	// Compare actual native and adapter admissions under the same frozen plan.
	nativePlan := imageSDKPlan(t, imagecompat.GenerateModel)
	_, err = engine.Submit(
		context.Background(),
		"native-parity",
		"g",
		1,
		[]byte(
			`{"model":"`+imagecompat.GenerateModel+`","input":{"num_images":1,"prompt":"draw fixture"}}`,
		),
		nativePlan,
	)
	require.NoError(t, err)
	require.NoError(
		t,
		model.SaveNativeTaskResult(
			db,
			"native-parity",
			"g",
			1,
			[]byte(`{"images":[{"url":"https://provider.invalid/image.png"}]}`),
		),
	)

	_, err = engine.Deliver(context.Background(), "native-parity", "g", 1, archive)
	require.NoError(t, err)

	headers := http.Header{}
	headers.Set("Idempotency-Key", "node-generate")
	adapterID, err := imagecompat.OperationID(headers, "g", 1)
	require.NoError(t, err)
	wallet.mu.Lock()
	defer wallet.mu.Unlock()

	nativeCommands := make([]balance.PrepaymentCommand, 0, len(wallet.commands))

	adapterCommands := make([]balance.PrepaymentCommand, 0, len(wallet.commands))
	for _, original := range wallet.commands {
		command := original //nolint:copyloopvar // Normalize a copy while retaining original identity for grouping.
		command.BillingOperationID = ""
		command.TaskID = ""
		command.UpstreamTaskID = ""

		command.AttemptID = ""
		if original.BillingOperationID == "native:native-parity" {
			nativeCommands = append(nativeCommands, command)
		}

		if original.BillingOperationID == "native:"+adapterID {
			adapterCommands = append(adapterCommands, command)
		}
	}

	require.Equal(
		t,
		nativeCommands,
		adapterCommands,
		"native and compatibility execute the same complete wallet command sequence",
	)

	var (
		nativeAmount   int64
		adapterAmounts []int64
	)
	for _, command := range wallet.commands {
		if command.Action == "admit" {
			if command.BillingOperationID == "native:native-parity" {
				nativeAmount = *command.PrepaidMicros
			} else {
				adapterAmounts = append(adapterAmounts, *command.PrepaidMicros)
			}
		}
	}

	require.NotEmpty(t, adapterAmounts)

	for _, amount := range adapterAmounts {
		require.Equal(t, nativeAmount, amount)
	}
}
