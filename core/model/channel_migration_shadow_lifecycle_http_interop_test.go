package model_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// Opt-in real Gin server, isolated SQLite and synthetic credentials only.
// The caller may cancel created/consumed and recover over GET without generation.
func TestMigrationShadowLifecycleHTTPInteropServer(t *testing.T) {
	dir := os.Getenv("B2_SHADOW_LIFECYCLE_HTTP_FIXTURE_DIR")
	if dir == "" {
		t.Skip("explicit isolated lifecycle interop directory required")
	}

	require.True(t, strings.HasPrefix(dir, "/private/tmp/"))
	f := newAdmissionFixture(t)
	template := shadowFixture(t, f)
	ctx := context.Background()
	operations := map[string]any{}
	// Created and cancelled resources do not reserve a Prepared family. Keep the
	// consumed case last; do not bypass the production overlapping-family guard.
	for _, phase := range []string{"created", "cancelled", "consumed"} {
		inv := f.store.ReadMigrationInventory(ctx, nil)
		require.Equal(t, "ready", inv.State)

		source := template
		source.OperationID = "interop-" + phase
		source.Source.SourceSnapshotHash = inv.SourceSnapshotHash
		source.Source.Revision = inv.Revision
		source.Target.Projection.Name = "own-" + phase
		got := f.store.CreateMigrationShadow(ctx, source, shadowTestKey, nil)
		require.Equal(t, "ready", got.State)

		if phase != "created" {
			snapshot, e := f.store.ReadMigrationSnapshot(ctx, got.Receipt.From, got.Receipt.Shadow)
			require.NoError(t, e)

			req := model.ChannelMigrationRequest{
				Seal: model.ChannelSealRequest{
					OperationID:      source.OperationID,
					Epoch:            snapshot.Epoch,
					ExpectedRevision: snapshot.Revision,
					From:             got.Receipt.From,
					To:               got.Receipt.Shadow,
				},
				FromCredentialID: "old",
				ToCredentialID:   source.Target.CredentialID,
				ExpectedPinHash:  snapshot.PinHash,
				ProjectionHash:   snapshot.ProjectionHash,
				FamilyHash:       snapshot.FamilyHash,
			}
			raw, e := json.Marshal(req)
			require.NoError(t, e)

			sum := sha256.Sum256(raw)
			evidence := sha256.Sum256([]byte("synthetic-evidence-" + phase))
			probe := model.VerifiedMigrationProbe{
				RequestHash:       hex.EncodeToString(sum[:]),
				EvidenceHash:      hex.EncodeToString(evidence[:]),
				StorageGeneration: f.proof.StorageGeneration,
				IssuedAtMs:        time.Now().Add(-time.Minute).UnixMilli(),
				ValidUntilMs:      time.Now().Add(time.Minute).UnixMilli(),
			}
			hash, e := probe.Fingerprint()
			require.NoError(t, e)
			require.NoError(
				t,
				f.store.DB.Create(
					&model.ChannelMigrationProbeRecord{
						EvidenceHash: probe.EvidenceHash,
						FactHash:     hash,
					},
				).Error,
			)
			f.store.LoadMigrationProbe = func(context.Context, model.ChannelMigrationRequest) (model.VerifiedMigrationProbe, error) {
				return probe, nil
			}
			_, e = f.store.PrepareMigration(ctx, req)
			require.NoError(t, e)

			if phase == "cancelled" {
				_, e = f.store.CancelMigrationShadow(
					ctx,
					source.OperationID,
					got.Receipt.RequestHash,
					got.Receipt.CheckpointHash,
				)
				require.NoError(t, e)
			}
		}

		operations[phase] = map[string]any{
			"operationId": source.OperationID,
			"creation":    got.Receipt,
			"cancelBody": map[string]any{
				"version":             2,
				"resourceRequestHash": got.Receipt.RequestHash,
				"checkpointHash":      got.Receipt.CheckpointHash,
			},
		}
	}

	for phase := range operations {
		got := f.store.ReadMigrationShadowLifecycleFacts(ctx, "interop-"+phase)
		require.Equal(t, "ready", got.State)
		require.Equal(t, phase, got.Observation.Lifecycle.State)
	}

	server := httptest.NewServer(lifecycleHTTPRouter(t, f))
	defer server.Close()

	//nolint:gosec // Local integration harness path comes from the test operator, never an HTTP request.
	require.NoError(t, os.MkdirAll(dir, 0o700))

	raw, e := json.Marshal(
		map[string]any{
			"baseUrl":           server.URL,
			"adminKey":          config.AdminKey,
			"operations":        operations,
			"absentOperationId": "interop-absent",
		},
	)
	require.NoError(t, e)

	file := filepath.Join(dir, "connection.json")

	//nolint:gosec // Local integration harness path comes from the test operator, never an HTTP request.
	require.NoError(t, os.WriteFile(file, raw, 0o600))
	defer os.Remove(file)

	until := time.Now().Add(10 * time.Minute)
	for time.Now().Before(until) {
		//nolint:gosec // Local integration harness path comes from the test operator, never an HTTP request.
		if _, e := os.Stat(filepath.Join(dir, "stop")); e == nil {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatal("lifecycle interop server was not stopped")
}

// Fresh source using the accepted real publisher export, for the application
// builder -> actual create -> lifecycle -> cancel service integration.
func TestMigrationShadowLifecycleHTTPNativeBuilderServer(t *testing.T) {
	dir := os.Getenv("B2_SHADOW_LIFECYCLE_HTTP_FIXTURE_DIR")
	if dir == "" {
		t.Skip("explicit isolated native builder interop directory required")
	}

	require.True(t, strings.HasPrefix(dir, "/private/tmp/"))
	f := newAdmissionFixture(t)
	_ = shadowFixture(t, f)
	raw, e := os.ReadFile("testdata/migration-inventory/publisher-builder.json")
	require.NoError(t, e)

	var publication struct {
		Config struct {
			Owner  string          `json:"owner"`
			Type   int             `json:"type"`
			Config json.RawMessage `json:"config"`
		} `json:"config"`
	}
	require.NoError(t, json.Unmarshal(raw, &publication))

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
	// No operation has been created. The application consumes actual HTTP
	// inventory and sends its independently frozen source/request using its key.
	server := httptest.NewServer(lifecycleHTTPRouter(t, f))
	defer server.Close()

	//nolint:gosec // Local integration harness path comes from the test operator, never an HTTP request.
	require.NoError(t, os.MkdirAll(dir, 0o700))

	raw, e = json.Marshal(
		map[string]any{
			"baseUrl":           server.URL,
			"adminKey":          config.AdminKey,
			"kind":              "native-builder",
			"oldChannelId":      11,
			"operationId":       "interop-native-builder",
			"absentOperationId": "interop-native-absent",
		},
	)
	require.NoError(t, e)

	file := filepath.Join(dir, "connection.json")

	//nolint:gosec // Local integration harness path comes from the test operator, never an HTTP request.
	require.NoError(t, os.WriteFile(file, raw, 0o600))
	defer os.Remove(file)

	until := time.Now().Add(10 * time.Minute)
	for time.Now().Before(until) {
		//nolint:gosec // Local integration harness path comes from the test operator, never an HTTP request.
		if _, e := os.Stat(filepath.Join(dir, "stop")); e == nil {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatal("native builder interop server was not stopped")
}
