package model_test

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"testing"

	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// These are actual prepareNativeGatewayPublication outputs, exported by invoking
// accepted app b36c59b4 publisher code with its existing isolated fixtures. No
// provider calls or database reads were used to generate them; see provenance.
func TestMigrationInventoryActualPublisherRoundtrip(t *testing.T) {
	for _, kind := range []string{"ordinary", "video", "image", "tts"} {
		t.Run(kind, func(t *testing.T) {
			raw, e := os.ReadFile("testdata/migration-inventory/publisher-" + kind + ".json")
			require.NoError(t, e)

			var publication struct {
				Model  string `json:"model"`
				Config struct {
					Owner  string          `json:"owner"`
					Type   int             `json:"type"`
					Config json.RawMessage `json:"config"`
				} `json:"config"`
			}
			require.NoError(t, json.Unmarshal(raw, &publication))

			var configs map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(publication.Config.Config, &configs))

			var binding struct {
				Contract       json.RawMessage `json:"contract"`
				ChannelID      int             `json:"channelId"`
				KeyFingerprint string          `json:"keyFingerprint"`
			}
			require.NoError(
				t,
				json.Unmarshal(configs["x_token_platform_native_result_v1"], &binding),
			)
			_, e = nativeresult.CompileTaskContract(binding.Contract)
			require.NoError(t, e)
			f := newAdmissionFixture(t)
			inventoryFixture(t, f)
			db := f.store.DB

			key := "native-test-key"
			if kind == "video" {
				key = "isolated-video-fixed-hold-key"
			}

			ch := model.Channel{
				ID:      binding.ChannelID,
				Name:    "publisher-fixture",
				Type:    model.ChannelTypeFal,
				Key:     key,
				BaseURL: "https://queue.fal.run",
			}
			require.NoError(t, db.Create(&ch).Error)
			id, e := model.ChannelIdentityOf(&ch)
			require.NoError(t, e)
			require.Equal(t, binding.KeyFingerprint, id.KeyDigest)
			require.NoError(
				t,
				db.Exec(
					"INSERT INTO model_configs (model,owner,type,config,plugin) VALUES (?,?,?,?,?)",
					publication.Model,
					publication.Config.Owner,
					publication.Config.Type,
					string(publication.Config.Config),
					"{}",
				).Error,
			)

			snap, e := f.store.ReadMigrationSnapshot(
				context.Background(),
				f.identities[0],
				f.identities[2],
			)
			require.NoError(t, e)

			got := f.store.ReadMigrationInventory(context.Background(), nil)
			require.Equal(t, "ready", got.State, got.ReasonCode)
			require.True(t, got.Complete)
			require.Equal(t, snap.PinHash, got.PinHash)

			found := false
			for _, config := range got.Configs {
				if config.Model == publication.Model {
					var row struct {
						Config string `json:"config"`
					}
					require.NoError(t, json.Unmarshal([]byte(config.RawJSON), &row))
					require.Equal(t, string(publication.Config.Config), row.Config)

					found = true
				}
			}

			require.True(t, found)

			for _, description := range []string{key, "Document link https://user:password@example.invalid/private"} {
				var contract map[string]any
				require.NoError(t, json.Unmarshal(binding.Contract, &contract))
				inputSchema, ok := contract["input_schema"].(map[string]any)
				require.True(t, ok)

				inputSchema["description"] = description
				altered, e := json.Marshal(contract)
				require.NoError(t, e)
				_, e = nativeresult.CompileTaskContract(altered)
				require.NoError(t, e)

				var b map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(configs["x_token_platform_native_result_v1"], &b))
				b["contract"] = altered

				copyConfig := map[string]json.RawMessage{}
				maps.Copy(copyConfig, configs)

				copyConfig["x_token_platform_native_result_v1"], e = json.Marshal(b)
				require.NoError(t, e)
				altered, e = json.Marshal(copyConfig)
				require.NoError(t, e)
				require.NoError(
					t,
					db.Exec(
						"UPDATE model_configs SET config=? WHERE model=?",
						string(altered),
						publication.Model,
					).Error,
				)

				denied := f.store.ReadMigrationInventory(context.Background(), nil)
				require.Equal(t, "unsupported", denied.State)
				require.Empty(t, denied.Configs)
			}
			// A structurally valid native contract is not permission to freeze credentials.
			var contract map[string]any
			require.NoError(t, json.Unmarshal(binding.Contract, &contract))
			contract["fixed_parameters"] = map[string]any{
				"authentication_token": "unrelated-private-token",
			}
			contract["upstream_input_schema"] = map[string]any{"type": "object"}
			bad, e := json.Marshal(contract)
			require.NoError(t, e)
			_, e = nativeresult.CompileTaskContract(bad)
			require.NoError(t, e)

			var object map[string]json.RawMessage
			require.NoError(
				t,
				json.Unmarshal(configs["x_token_platform_native_result_v1"], &object),
			)
			object["contract"] = bad
			configs["x_token_platform_native_result_v1"], e = json.Marshal(object)
			require.NoError(t, e)
			bad, e = json.Marshal(configs)
			require.NoError(t, e)
			require.NoError(
				t,
				db.Exec(
					"UPDATE model_configs SET config=? WHERE model=?",
					string(bad),
					publication.Model,
				).Error,
			)

			denied := f.store.ReadMigrationInventory(context.Background(), nil)
			require.Equal(t, "unsupported", denied.State)
			require.Empty(t, denied.Configs)
		})
	}
}

func TestMigrationInventoryActualPublisherBuilderWire(t *testing.T) {
	raw, e := os.ReadFile("testdata/migration-inventory/publisher-builder.json")
	require.NoError(t, e)

	var publication struct {
		Model  string `json:"model"`
		Config struct {
			Owner  string          `json:"owner"`
			Type   int             `json:"type"`
			Config json.RawMessage `json:"config"`
		} `json:"config"`
	}
	require.NoError(t, json.Unmarshal(raw, &publication))
	f := newAdmissionFixture(t)
	inventoryFixture(t, f)
	db := f.store.DB
	require.NoError(t, db.Exec("DELETE FROM channels").Error)
	require.NoError(t, db.Exec("DELETE FROM model_configs").Error)

	ch := model.Channel{
		ID:           11,
		Name:         "Primary [shadow v1-logical]",
		Type:         model.ChannelTypeFal,
		Key:          "synthetic-preparation-old",
		BaseURL:      "https://queue.fal.run",
		Status:       1,
		Models:       []string{"test/native::text-to-video"},
		ModelMapping: map[string]string{"test/native::text-to-video": "fal-ai/fixture"},
		Sets:         []string{"default"},
		Priority:     1,
		Configs:      model.ChannelConfigs{"outbound_policy": "public_only"},
	}
	require.NoError(t, db.Create(&ch).Error)
	require.NoError(
		t,
		db.Exec(
			"INSERT INTO model_configs (model,owner,type,config,plugin) VALUES (?,?,?,?,?)",
			"test/native::text-to-video",
			publication.Config.Owner,
			publication.Config.Type,
			string(publication.Config.Config),
			"{}",
		).Error,
	)

	got := f.store.ReadMigrationInventory(
		context.Background(),
		[]string{"channel_routing_guard_v1", "registry_provider_fal_async_v1"},
	)
	require.Equal(t, "ready", got.State, got.ReasonCode)
	require.True(t, got.Complete)
	require.Len(t, got.Configs, 1)
	require.Len(t, got.Channels, 1)

	if path := os.Getenv("B2_INVENTORY_REVIEW_WIRE"); path != "" {
		require.Equal(t, "/private/tmp/b2-inventory-native-ready.json", path)

		encoded, e := json.MarshalIndent(got, "", "  ")
		require.NoError(t, e)
		//nolint:gosec // Local integration harness path comes from the test operator, never an HTTP request.
		require.NoError(t, os.WriteFile(path, encoded, 0o600))
	}
}
