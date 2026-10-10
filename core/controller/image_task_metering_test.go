//nolint:testpackage
package controller

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/imageprepayment"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func imageMeteringMap(t *testing.T, value any) map[string]any {
	t.Helper()

	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T", value)
	}

	return result
}

func TestImageTaskMeasuredAdmissionBeforePaidSubmit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		balance     float64
		version     int
		conditional bool
		status      int
		undeclared  bool
	}{
		{"real wallet prepaid", 0, 1, false, 202, false}, {"prepaid", 0, 1, false, 202, false}, {"admin demo", 0, 1, false, 202, false}, {"accepted", 2, 1, false, 202, false}, {"insufficient", .5, 1, false, 402, false}, {"bad metadata", 2, 2, false, 400, false}, {"conditional", 2, 1, true, 400, false}, {"undeclared references with positive input rate", 2, 1, false, 400, true}, {"text only with positive input rate", 2, 1, false, 202, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			textOnly := tc.name == "text only with positive input rate"
			adminDemo := tc.name == "admin demo"
			realWallet := tc.name == "real wallet prepaid"
			walletURL := os.Getenv("D34_TEST_WALLET_URL")
			if realWallet && walletURL == "" {
				t.Skip("requires loopback prepayment-wallet-server.ts fixture")
			}
			if realWallet {
				require.True(t, strings.HasPrefix(walletURL, "http://127.0.0.1:"))
			}
			groupID := "g"
			if realWallet {
				groupID = "u_u"
			}
			prepaid := tc.name == "prepaid" || realWallet
			prepaymentActions := []string{}
			if prepaid {
				wallet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, "/api/internal/wallet/prepayment", r.URL.Path)
					var command balance.PrepaymentCommand
					require.NoError(t, json.NewDecoder(r.Body).Decode(&command))
					prepaymentActions = append(prepaymentActions, command.Action)
					if realWallet {
						raw, err := json.Marshal(command)
						require.NoError(t, err)
						req, err := http.NewRequest(http.MethodPost, walletURL+r.URL.Path, bytes.NewReader(raw))
						require.NoError(t, err)
						req.Header.Set("Authorization", "Bearer local-d34-test-only")
						resp, err := http.DefaultClient.Do(req)
						require.NoError(t, err)
						defer resp.Body.Close()
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(resp.StatusCode)
						_, err = io.Copy(w, resp.Body)
						require.NoError(t, err)
						return
					}
					receipt := map[string]any{"id": command.BillingOperationID, "status": "pending", "prepaidMicros": 2000000}
					if command.Action == "begin_attempt" {
						receipt["claimed"] = true
					}
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": receipt}))
				}))
				defer wallet.Close()
				oldBalance := balance.Default
				balance.Default = balance.NewExternalHTTP(wallet.URL, "local-only")
				t.Cleanup(func() { balance.Default = oldBalance })
			}
			raw, err := os.ReadFile("../common/registryvalidation/testdata/provider.json")
			require.NoError(t, err)

			var contract map[string]any
			require.NoError(t, json.Unmarshal(raw, &contract))
			contract["execution"] = map[string]any{"mode": "async", "output": "image"}
			provider := imageMeteringMap(t, imageMeteringMap(t, contract["providers"])["small"])
			upstream := imageMeteringMap(t, provider["upstream"])

			upstream["metering"] = map[string]any{
				"version":                        tc.version,
				"inputImagesParameter":           "image_urls",
				"maxInputImages":                 3,
				"outputCountMultiplierParameter": "max_images",
				"maxCombinedImages":              8,
			}
			if tc.undeclared {
				metering := imageMeteringMap(t, upstream["metering"])
				delete(metering, "inputImagesParameter")
				delete(metering, "maxInputImages")
			}

			for _, schema := range []map[string]any{imageMeteringMap(t, contract["input_schema"]), imageMeteringMap(t, upstream["acceptedInputJsonSchema"]), imageMeteringMap(t, upstream["inputJsonSchema"])} {
				properties := imageMeteringMap(t, schema["properties"])
				properties["image_urls"] = map[string]any{
					"type":     "array",
					"items":    map[string]any{"type": "string"},
					"minItems": 1,
					"maxItems": 3,
				}
				properties["max_images"] = map[string]any{
					"type":    "integer",
					"minimum": 1,
					"maximum": 3,
				}
			}

			provider["allowedPassthroughParameters"] = []string{"image_urls", "max_images"}
			db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "submit.db"))
			require.NoError(t, err)
			require.NoError(
				t,
				db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}),
			)

			old := model.LogDB
			model.LogDB = db
			t.Cleanup(func() { model.LogDB = old })

			calls := 0

			upstreamServer := httptest.NewServer(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if prepaid {
						require.Equal(t, []string{"admit", "begin_attempt"}, prepaymentActions)
					}

					require.Equal(t, http.MethodPost, r.Method)

					var sent map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&sent))
					require.Equal(t, float64(3), sent["max_images"])

					var stored model.AsyncUsageInfo
					require.NoError(t, db.First(&stored).Error)
					require.Equal(t, adminDemo, stored.InternalImageTask)
					if adminDemo {
						require.Equal(t, model.Price{}, stored.Price)
						var entry model.Log
						require.NoError(t, db.First(&entry).Error)
						require.Equal(t, model.RequestSourceAdminDemo, entry.RequestSource)
					}

					if textOnly {
						require.Zero(t, *stored.UsageContext.ImageUsage.InputCount)
					} else if !tc.undeclared {
						require.Equal(t, int64(2), *stored.UsageContext.ImageUsage.InputCount)
					}

					var task model.ImageTask
					require.NoError(t, db.First(&task).Error)
					require.Equal(t, 3, task.ExpectedImages)

					_, _ = w.Write([]byte(`{"request_id":"upstream"}`))
				}),
			)
			defer upstreamServer.Close()

			binding := map[string]any{
				"provider":     "small",
				"id":           "small",
				"revision":     "1",
				"contractHash": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			}
			ch := &model.Channel{
				ID:           7,
				Type:         model.ChannelTypeFal,
				Key:          "secret",
				BaseURL:      upstreamServer.URL,
				Models:       []string{"image"},
				ModelMapping: map[string]string{"image": "fal-ai/test"},
				Configs: map[string]any{
					providerBindingsConfig: map[string]any{"image": binding},
				},
			}
			maxPixels := int64(100)

			price := model.Price{
				ImageBilling: &model.ImageBillingPolicy{
					Version:  1,
					Scenario: "generation",
					Input: &model.ImageBillingInput{
						ChargeBasis:  "per_output",
						FirstNFree:   1,
						AmountMicros: 50000,
						UnitQuantity: 1,
					},
					OutputPixelTiers: []model.ImageBillingTier{
						{MaxPixels: &maxPixels, AmountMicros: 400000, UnitQuantity: 1},
						{AmountMicros: 200000, UnitQuantity: 1},
					},
				},
			}
			if tc.conditional {
				price.ConditionalPrices = []model.ConditionalPrice{{}}
			}

			for range 2 {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)

				body := `{"model":"image","prompt":"hi","n":1,"max_images":3,"image_urls":["https://img/a","https://img/b"]}`
				if textOnly {
					body = `{"model":"image","prompt":"hi","n":1,"max_images":3}`
				}

				c.Request = httptest.NewRequestWithContext(
					t.Context(),
					http.MethodPost,
					"/v1/images/tasks",
					bytes.NewBufferString(body),
				)
				c.Request.Header.Set("Content-Type", "application/json")
				c.Request.Header.Set("X-Request-ID", "metered-request")
				if adminDemo {
					c.Request.Header.Set(middleware.OperationalLogSourceHeader, model.RequestSourceAdminDemo)
				}
				c.Request.Header.Set(AIProxyChannelHeader, "7")
				c.Set(
					middleware.Group,
					model.GroupCache{ID: groupID, Status: model.GroupStatusInternal},
				)
				c.Set(middleware.Token, model.TokenCache{ID: 1})
				c.Set(middleware.RequestModel, "image")
				c.Set(middleware.RoutingModel, "image")
				c.Set(middleware.RequestedModel, "image")
				c.Set(
					middleware.ModelCaches,
					&model.ModelCaches{ChannelsByID: map[int]*model.Channel{7: ch}},
				)
				c.Set(
					middleware.ModelConfig,
					model.ModelConfig{
						Price: price,
						Config: map[model.ModelConfigKey]any{
							"x_token_platform_capability_contract": map[string]any{
								"entry_id": "image",
								"contract": contract,
							},
							"x_token_platform_pricing": map[string]any{
								"currency":        "USD",
								"pricing_version": "v1",
							},
						},
					},
				)
				if prepaid {
					mc := middleware.GetModelConfig(c)
					var quote map[string]any
					require.NoError(t, json.Unmarshal([]byte(`{"version":1,"quoteVersion":"test-1","currency":"USD","prepaidMicros":2000000,"routes":[{"routeId":"primary","channelId":7,"provider":"fal","endpoint":"fal-ai/test","credentialScope":"channel-7","estimatedMicros":1000000,"prepaidMicros":2000000,"rule":{"mode":"list_ratio","ratio":"1"}}]}`), &quote))
					mc.Config[model.ImagePrepaymentConfigKey] = quote
					c.Set(middleware.ModelConfig, mc)
				}
				c.Set(
					middleware.GroupBalance,
					&middleware.GroupBalanceConsumer{
						Group: "g",
						CheckBalance: func(required float64) bool {
							if adminDemo || prepaid {
								t.Fatal("admin image task must not check customer wallet")
							}
							if textOnly {
								require.InDelta(t, 1.2, required, 0.000001)
							} else if !tc.undeclared {
								require.InDelta(t, 1.35, required, 0.000001)
							}

							return tc.balance >= required
						},
					},
				)
				submitImageTask(c)
				require.Equal(t, tc.status, w.Code, w.Body.String())
			}

			if tc.status == 202 {
				require.Equal(t, 1, calls)
				if prepaid {
					require.Equal(t, []string{"admit", "begin_attempt", "accept_attempt", "get"}, prepaymentActions)
					if realWallet {
						var stored model.ImageTask
						require.NoError(t, db.First(&stored).Error)
						stored.Status = "completed"
						require.NoError(t, db.Save(&stored).Error)
						_, err := imageprepayment.Sync(t.Context(), &stored)
						require.NoError(t, err)
						post := func(path string, value any) json.RawMessage {
							raw, err := json.Marshal(value)
							require.NoError(t, err)
							req, err := http.NewRequest(http.MethodPost, walletURL+path, bytes.NewReader(raw))
							require.NoError(t, err)
							req.Header.Set("Authorization", "Bearer local-d34-test-only")
							resp, err := http.DefaultClient.Do(req)
							require.NoError(t, err)
							defer resp.Body.Close()
							var result struct {
								Code    int             `json:"code"`
								Data    json.RawMessage `json:"data"`
								Message string          `json:"message"`
							}
							require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
							require.Equal(t, 0, result.Code, result.Message)
							return result.Data
						}
						post("/_test/bill", map[string]any{"provider": "fal", "requestId": stored.UpstreamID, "endpoint": stored.UpstreamModel, "currency": "USD", "subtotal": "0.3", "total": "0.2"})
						post("/api/internal/wallet/reconcile", nil)
						post("/api/internal/wallet/reconcile", nil)
						receipt, err := imageprepayment.Sync(t.Context(), &stored)
						require.NoError(t, err)
						require.NotNil(t, receipt.ChargedMicros)
						require.Equal(t, int64(300000), *receipt.ChargedMicros)
						var ledger []struct {
							RequestID string `json:"request_id"`
							Amount    int64  `json:"amount_micros"`
						}
						require.NoError(t, json.Unmarshal(post("/_test/ledger", nil), &ledger))
						count := 0
						net := int64(0)
						for _, r := range ledger {
							if r.RequestID == "prepay:"+stored.BillingOperationID || r.RequestID == "prepay-refund:"+stored.BillingOperationID {
								count++
								net += r.Amount
							}
						}
						require.Equal(t, 2, count)
						require.Equal(t, int64(-300000), net)
					}
				}
			} else {
				require.Zero(t, calls)

				var count int64
				require.NoError(t, db.Model(&model.ImageTask{}).Count(&count).Error)
				require.Zero(t, count)
			}
		})
	}
}
