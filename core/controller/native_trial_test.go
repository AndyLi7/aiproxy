package controller

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

type trialWallet struct{ calls []string }

func (w *trialWallet) Prepayment(_ context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	w.calls = append(w.calls, c.Action)
	return balance.PrepaymentReceipt{ID: c.BillingOperationID, Claimed: true}, nil
}

type trialProvider struct {
	calls int
	input string
}

func (p *trialProvider) SubmitNative(_ context.Context, _ string, body []byte) (string, error) {
	p.calls++
	p.input = string(body)
	return "private-upstream", nil
}
func TestNativePrivateTrialIsolatedAdmissionReplayAndDelivery(t *testing.T) {
	database, err := model.OpenSQLite(filepath.Join(t.TempDir(), "trial.db"))
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.NativeTask{}))
	sqlDB, err := database.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	wallet, provider := &trialWallet{}, &trialProvider{}
	engine := &nativetask.Engine{DB: database, Wallet: wallet}
	group := &model.Group{ID: "internal", Status: model.GroupStatusInternal}
	channel := &model.Channel{ID: 9, Type: model.ChannelTypeFal, Status: model.ChannelStatusDisabled, Key: "trial-secret", BaseURL: "https://queue.fal.run"}
	deps := nativeTrialDependencies{
		engine: func() (*nativetask.Engine, bool) { return engine, true },
		token: func(id int) (*model.Token, error) {
			return &model.Token{ID: id, GroupID: "internal", Status: model.TokenStatusEnabled}, nil
		},
		group: func(string, bool) (*model.Group, error) { return group, nil }, channel: func(int) (*model.Channel, error) { return channel, nil },
		provider: func(*model.Channel) nativetask.Provider { return provider },
	}
	oldKey := config.AdminKey
	config.AdminKey = "private-admin-test"
	t.Cleanup(func() { config.AdminKey = oldKey })
	router := gin.New()
	api := router.Group("/api", middleware.AdminAuth)
	api.POST("/native-trials/:group/:token", deps.create)
	api.GET("/native-trials/:group/:token/:id", func(c *gin.Context) { deps.read(c, false) })
	call := func(method, path, body, key, id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Request-Id", id)
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	input := nativeTrialRequest{Contract: json.RawMessage(`{"version":1,"model":"test/model/native","input_schema":{"type":"object","required":["seed"],"properties":{"seed":{"type":"integer"}},"additionalProperties":false},"output_schema":{"type":"null"}}`), Input: json.RawMessage(`{"seed":9007199254740993}`), ChannelID: 9, Endpoint: "fal-ai/native/variant", CredentialScope: "scope", KeyFingerprint: model.ImageChannelKeyFingerprint(channel.Key), DeliveryBase: "https://gateway.example",
		Quote: json.RawMessage(`{"version":1,"currency":"USD","quoteVersion":"private-fixture","prepaidMicros":200,"routes":[{"routeId":"private","channelId":9,"provider":"fal","endpoint":"fal-ai/native/variant","credentialScope":"scope","quantityMetric":"request","estimatedMicros":100,"prepaidMicros":200,"rule":{"mode":"cost_markup","ratio":"1"}}]}`)}
	encoded, _ := json.Marshal(input)
	body := string(encoded)
	path := "/api/native-trials/internal/7"
	require.Equal(t, 401, call("POST", path, body, "public-key", "trial_1").Code)
	require.Equal(t, 403, call("POST", "/api/native-trials/customer/7", body, config.AdminKey, "trial_1").Code)
	group.Status = model.GroupStatusEnabled
	require.Equal(t, 403, call("POST", path, body, config.AdminKey, "trial_1").Code)
	group.Status = model.GroupStatusInternal
	for _, bad := range []string{body + `{}`, `{"input":{},"input":{}}`, strings.Replace(body, `"seed":9007199254740993`, `"seed":"invalid"`, 1), strings.Replace(body, input.KeyFingerprint, strings.Repeat("0", 64), 1), strings.Replace(body, `https://gateway.example`, `http://gateway.example`, 1)} {
		out := call("POST", path, bad, config.AdminKey, "trial_1")
		require.Equal(t, 400, out.Code, out.Body.String())
	}
	require.Empty(t, wallet.calls)
	require.Zero(t, provider.calls)
	for i := 0; i < 2; i++ {
		out := call("POST", path, body, config.AdminKey, "trial_1")
		require.Equal(t, 202, out.Code, out.Body.String())
		require.NotContains(t, out.Body.String(), "private-upstream")
	}
	require.Equal(t, 1, provider.calls)
	require.Contains(t, provider.input, "9007199254740993")
	require.Equal(t, 1, strings.Count(strings.Join(wallet.calls, ","), "begin_attempt"))
	changed := strings.Replace(body, `"seed":9007199254740993`, `"seed":2`, 1)
	require.Equal(t, 409, call("POST", path, changed, config.AdminKey, "trial_1").Code)
	require.Equal(t, 1, provider.calls)
	require.NoError(t, model.SaveNativeTaskResult(database, "trial_1", "internal", 7, []byte("null")))
	task, err := engine.Deliver(context.Background(), "trial_1", "internal", 7, nil)
	require.NoError(t, err)
	require.Equal(t, "completed", task.Status)
	out := call("GET", path+"/trial_1", "", config.AdminKey, "")
	require.Equal(t, 200, out.Code, out.Body.String())
	require.Contains(t, out.Body.String(), `"outputJSON":"null"`)
	require.NotContains(t, out.Body.String(), "trial-secret")
	require.NotContains(t, out.Body.String(), "credentialScope")
	require.Equal(t, 404, call("GET", "/api/native-trials/internal/8/trial_1", "", config.AdminKey, "").Code)
	require.False(t, database.Migrator().HasTable(&model.ModelConfig{}), "private trials do not create public model configuration")
}

// Synthetic receipts exercise the gate, not provider billing certification.
func TestNativeTrialVerificationRequiresActualBoundSettlement(t *testing.T) {
	for _, tc := range []struct {
		name, receipt, output string
		settled, want         bool
	}{
		{"actual", `{"id":"bill","status":"settled","chargedMicros":10}`, `null`, true, true},
		{"zero", `{"id":"bill","status":"settled","chargedMicros":0}`, `null`, true, true},
		{"estimate", `{"id":"bill","status":"estimated","chargedMicros":10}`, `null`, true, false},
		{"refund", `{"id":"bill","status":"refunded","chargedMicros":0}`, `null`, true, false},
		{"wrong-operation", `{"id":"other","status":"settled","chargedMicros":10}`, `null`, true, false},
		{"missing-charge", `{"id":"bill","status":"settled"}`, `null`, true, false},
		{"negative", `{"id":"bill","status":"settled","chargedMicros":-1}`, `null`, true, false},
		{"not-terminal", `{"id":"bill","status":"settled","chargedMicros":10}`, `null`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(out)
			task := &model.NativeTask{ID: "trial_test", Model: "test/model/native", Status: "completed", UpstreamID: "upstream", BillingOperationID: "bill", BillingSettled: tc.settled, BillingReceiptJSON: tc.receipt, DeliveredOutput: tc.output, FrozenContract: `{"version":1,"model":"test/model/native","input_schema":{"type":"object"},"output_schema":{"type":"null"}}`}
			writeNativeTrial(c, 200, task)
			var response struct {
				Data struct {
					Verification map[string]bool `json:"verification"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(out.Body.Bytes(), &response))
			require.Equal(t, tc.want, response.Data.Verification["billingVerified"])
			require.True(t, response.Data.Verification["deliveryVerified"])
			task.DeliveredOutput = `{}`
			out = httptest.NewRecorder()
			c, _ = gin.CreateTestContext(out)
			writeNativeTrial(c, 200, task)
			require.NoError(t, json.Unmarshal(out.Body.Bytes(), &response))
			require.False(t, response.Data.Verification["deliveryVerified"])
		})
	}
}

func TestNativeTrialArtifactListRequiresBoundManifestAndOwnedOutput(t *testing.T) {
	hash := strings.Repeat("a", 64)
	task := &model.NativeTask{ID: "trial_files", Status: "completed", DeliveryBase: "https://gateway.example",
		FrozenContract:   `{"version":1,"model":"test/file/native","input_schema":{"type":"object"},"output_schema":{"type":"object"},"artifacts":[{"path":["file"]}]}`,
		DeliveredOutput:  `{"file":"https://gateway.example/v1/model-tasks/trial_files/artifacts/0"}`,
		ArtifactManifest: `{"0":{"key":"native-results/trial_files/0/` + hash + `.bin","sha256":"` + hash + `","size":42}}`}
	list, valid := nativeTrialArtifactList(task)
	require.True(t, valid)
	require.Equal(t, []gin.H{{"index": 0, "size": int64(42)}}, list)
	for _, change := range []string{"missing", "wrong-owner", "wrong-url", "pending"} {
		copy := *task
		switch change {
		case "missing":
			copy.ArtifactManifest = "{}"
		case "wrong-owner":
			copy.ArtifactManifest = strings.Replace(copy.ArtifactManifest, "trial_files/0/", "other/0/", 1)
		case "wrong-url":
			copy.DeliveredOutput = `{"file":"https://provider.example/raw.zip"}`
		case "pending":
			copy.Status = "running"
		}
		list, valid = nativeTrialArtifactList(&copy)
		require.False(t, valid, change)
		require.Empty(t, list, change)
	}
}

// Trial evidence names the rejected fields of an invalid_parameters task.
func TestNativeTrialListsRejectedParameters(t *testing.T) {
	task := &model.NativeTask{ID: "trial_voice", Model: "test/model/native", Status: "failed", ErrorCode: "invalid_parameters",
		PublicError:    `{"issues":[{"field":"voice","rule":"unsupported_value"}]}`,
		FrozenContract: `{"version":1,"model":"test/model/native","input_schema":{"type":"object"},"output_schema":{"type":"null"}}`}
	out := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(out)
	writeNativeTrial(c, 200, task)
	var response struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Body.Bytes(), &response))
	require.JSONEq(t, `"invalid_parameters"`, string(response.Data["errorCode"]))
	require.JSONEq(t, `[{"field":"voice","rule":"unsupported_value"}]`, string(response.Data["errorIssues"]))
	task.ErrorCode, task.PublicError = "upstream_task_failed", ""
	out = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(out)
	writeNativeTrial(c, 200, task)
	require.NotContains(t, out.Body.String(), "errorIssues")
}

type meterTrialWallet struct{ commands []balance.PrepaymentCommand }

func (w *meterTrialWallet) Prepayment(_ context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	w.commands = append(w.commands, c)
	return balance.PrepaymentReceipt{ID: c.BillingOperationID, Claimed: true}, nil
}

// Private trials go through the same input meter as public tasks: the trial
// request carries the meter next to its quote, and the engine flag applies.
func TestNativePrivateTrialSizesHoldWithInputMeter(t *testing.T) {
	quote := json.RawMessage(`{"currency":"USD","prepaidMicros":200000,"quoteVersion":"draft","routes":[{"channelId":9,"credentialScope":"scope","endpoint":"elevenlabs/tts/eleven-v4-turbo","estimatedMicros":200000,"prepaidMicros":200000,"provider":"fal","quantityMetric":"request","routeId":"turbo","rule":{"mode":"list_ratio","ratio":"1"}}],"settlementPolicy":"actual-cost-v1","version":1}`)
	meter := json.RawMessage(`{"version":1,"metric":"characters","path":["text"],"routes":{"turbo":{"unitSize":1000,"unitMicros":40000,"maxQuantity":5000,"maxMicros":200000}}}`)
	inline := json.RawMessage(`{"version":1,"model":"elevenlabs/tts/eleven-v4-turbo","input_schema":{"type":"object","required":["text"],"properties":{"text":{"type":"string","maxLength":5000},"voice":{"type":"string"}}},"output_schema":{"type":"object"}}`)
	// The contract the application publishes for Eleven v4 Turbo (a local
	// $ref request schema) and today's 122-character production text, shared
	// with the nativetask golden files.
	imported, err := os.ReadFile(filepath.Join("..", "common", "nativetask", "testdata", "contract-turbo.json"))
	require.NoError(t, err)
	text122, err := os.ReadFile(filepath.Join("..", "common", "nativetask", "testdata", "metered-input-turbo-122.json"))
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		meter    json.RawMessage
		enabled  bool
		hold     int64
		contract json.RawMessage
		input    json.RawMessage
	}{
		{"metered", meter, true, 200, inline, json.RawMessage(`{"text":"hello","voice":"Aria"}`)},
		{"no meter sent", nil, true, 200000, inline, json.RawMessage(`{"text":"hello","voice":"Aria"}`)},
		{"engine switch off", meter, false, 200000, inline, json.RawMessage(`{"text":"hello","voice":"Aria"}`)},
		{"imported contract", meter, true, 4880, json.RawMessage(imported), json.RawMessage(text122)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, err := model.OpenSQLite(filepath.Join(t.TempDir(), "trial.db"))
			require.NoError(t, err)
			require.NoError(t, database.AutoMigrate(&model.NativeTask{}))
			sqlDB, err := database.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = sqlDB.Close() })
			wallet, provider := &meterTrialWallet{}, &trialProvider{}
			engine := &nativetask.Engine{DB: database, Wallet: wallet, InputMeter: tc.enabled}
			channel := &model.Channel{ID: 9, Type: model.ChannelTypeFal, Status: model.ChannelStatusDisabled, Key: "trial-secret", BaseURL: "https://queue.fal.run"}
			deps := nativeTrialDependencies{
				engine: func() (*nativetask.Engine, bool) { return engine, true },
				token: func(id int) (*model.Token, error) {
					return &model.Token{ID: id, GroupID: "internal", Status: model.TokenStatusEnabled}, nil
				},
				group: func(string, bool) (*model.Group, error) {
					return &model.Group{ID: "internal", Status: model.GroupStatusInternal}, nil
				},
				channel:  func(int) (*model.Channel, error) { return channel, nil },
				provider: func(*model.Channel) nativetask.Provider { return provider },
			}
			router := gin.New()
			router.POST("/api/native-trials/:group/:token", deps.create)
			input := nativeTrialRequest{Contract: tc.contract, Input: tc.input, ChannelID: 9, Endpoint: "elevenlabs/tts/eleven-v4-turbo", CredentialScope: "scope",
				KeyFingerprint: model.ImageChannelKeyFingerprint(channel.Key), DeliveryBase: "https://gateway.example", Quote: quote, InputMeter: tc.meter}
			encoded, err := json.Marshal(input)
			require.NoError(t, err)
			require.Equal(t, tc.meter != nil, strings.Contains(string(encoded), `"inputMeter"`))
			request := httptest.NewRequest("POST", "/api/native-trials/internal/7", strings.NewReader(string(encoded)))
			request.Header.Set("X-Request-Id", "trial_meter")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			require.Equal(t, 202, response.Code, response.Body.String())
			require.NotEmpty(t, wallet.commands)
			admit := wallet.commands[0]
			require.Equal(t, "admit", admit.Action)
			require.Equal(t, tc.hold, *admit.PrepaidMicros)
			require.Equal(t, tc.hold, *admit.EstimatedMicros)
			if tc.hold == 200000 {
				require.Equal(t, string(quote), admit.QuoteJSON, "an unmetered trial forwards its quote byte for byte")
			}
			require.Equal(t, 1, provider.calls)
		})
	}
}
