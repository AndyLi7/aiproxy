package model_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func probeFixture(t *testing.T, f *admissionFixture) (model.MigrationProbeAuthorization, *model.MigrationShadowReceipt) {
	t.Helper()
	source := shadowFixture(t, f)
	db := f.store.DB
	raw, e := os.ReadFile("testdata/migration-inventory/publisher-builder.json")
	require.NoError(t, e)
	var pub struct {
		Config struct {
			Config json.RawMessage `json:"config"`
		} `json:"config"`
	}
	require.NoError(t, json.Unmarshal(raw, &pub))
	require.NoError(t, db.Exec("DELETE FROM channels").Error)
	require.NoError(t, db.Exec("DELETE FROM model_configs").Error)
	ch := model.Channel{ID: 11, Name: "Primary [shadow v1-logical]", Type: model.ChannelTypeFal, Key: "synthetic-preparation-old", BaseURL: "https://queue.fal.run", Status: 1, Models: []string{"test/native::text-to-video"}, ModelMapping: map[string]string{"test/native::text-to-video": "fal-ai/fixture"}, Sets: []string{"default"}, Priority: 1, Configs: model.ChannelConfigs{"outbound_policy": "public_only"}}
	require.NoError(t, db.Create(&ch).Error)
	require.NoError(t, db.Exec("INSERT INTO model_configs (model,config,plugin) VALUES (?,?,?)", "test/native::text-to-video", string(pub.Config.Config), "{}").Error)
	require.NoError(t, db.AutoMigrate(&model.ChannelMigrationProbeRuntimeProtocol{}, &model.ChannelMigrationProbeRun{}, &model.NativeTask{}, &model.Group{}, &model.Token{}))
	require.NoError(t, db.Create(&model.ChannelMigrationProbeRuntimeProtocol{ID: 1, Version: 1}).Error)
	require.NoError(t, db.Create(&model.Group{ID: "probe-internal", Status: model.GroupStatusInternal}).Error)
	require.NoError(t, db.Create(&model.Token{ID: 1, GroupID: "probe-internal", Status: model.TokenStatusEnabled, Name: "probe"}).Error)
	inv := f.store.ReadMigrationInventory(context.Background(), nil)
	require.Equal(t, "ready", inv.State, inv.ReasonCode)
	source.Source = model.MigrationShadowSource{SourceSnapshotHash: inv.SourceSnapshotHash, Epoch: inv.Epoch, Revision: inv.Revision, StorageGeneration: inv.StorageGeneration, From: inv.Channels[0].Identity}
	source.Target.Projection.Models = ch.Models
	source.Target.Projection.ModelMapping = ch.ModelMapping
	created := f.store.CreateMigrationShadow(context.Background(), source, shadowTestKey, nil)
	require.Equal(t, "ready", created.State, created.Reason)
	snap, e := f.store.ReadMigrationSnapshot(context.Background(), created.Receipt.From, created.Receipt.Shadow)
	require.NoError(t, e)
	req := model.ChannelMigrationRequest{Seal: model.ChannelSealRequest{OperationID: source.OperationID, Epoch: snap.Epoch, ExpectedRevision: snap.Revision, From: created.Receipt.From, To: created.Receipt.Shadow}, FromCredentialID: "credential-real-publisher", ToCredentialID: source.Target.CredentialID, ExpectedPinHash: snap.PinHash, ProjectionHash: snap.ProjectionHash, FamilyHash: snap.FamilyHash}
	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(pub.Config.Config, &cfg))
	var binding struct {
		CredentialScope string `json:"credentialScope"`
	}
	require.NoError(t, json.Unmarshal(cfg["x_token_platform_native_result_v1"], &binding))
	req.FromCredentialID = binding.CredentialScope
	return model.MigrationProbeAuthorization{Command: req, ResourceRequestHash: created.Receipt.RequestHash, CheckpointHash: created.Receipt.CheckpointHash, Model: "test/native::text-to-video", Input: json.RawMessage(`{"model":"test/native/text-to-video","input":{"prompt":"local synthetic probe","duration":"5"}}`), GroupID: "probe-internal", TokenID: 1, MaxPrepaidMicros: 9007199254740991}, created.Receipt
}
func probeNativeTask(t *testing.T, h *model.MigrationProbeHandle) model.NativeTask {
	s, e := h.Spec()
	require.NoError(t, e)
	var frozen nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(s.Contract, &frozen))
	c, e := nativeresult.CompileTaskContract(s.Contract)
	require.NoError(t, e)
	input, e := c.ValidateRequest(s.Input)
	require.NoError(t, e)
	input, e = c.PrepareUpstreamInput(input)
	require.NoError(t, e)
	hash := sha256.Sum256(s.Input)
	return model.NativeTask{ID: s.TaskID, GroupID: s.GroupID, TokenID: s.TokenID, Model: frozen.Model, Fingerprint: hex.EncodeToString(hash[:]), OutputSchema: string(frozen.OutputSchema), FrozenContract: string(s.Contract), NativeInput: string(input), ChannelID: s.ChannelID, Endpoint: s.Endpoint, CredentialScope: s.CredentialScope, PrepaymentQuoteJSON: s.QuoteJSON, BillingOperationID: "native:" + s.TaskID, DeliveryBase: s.DeliveryBase, KeyFingerprint: s.KeyDigest, AdmissionIdentityHash: s.IdentityHash, QueueScope: "fal"}
}
func TestMigrationProbeIntentAndAtomicNativeReservation(t *testing.T) {
	f := newAdmissionFixture(t)
	a, receipt := probeFixture(t, f)
	ctx := context.Background()
	h, e := f.store.BeginMigrationProbe(ctx, a)
	require.NoError(t, e)
	replay, e := f.store.BeginMigrationProbe(ctx, a)
	require.NoError(t, e)
	first, _ := h.Spec()
	second, _ := replay.Spec()
	require.Equal(t, first, second)
	_, _, e = f.store.Admit(ctx, admissionRequest("ordinary", receipt.Shadow), nil)
	require.ErrorIs(t, e, model.ErrChannelAdmissionSealed)
	task := probeNativeTask(t, h)
	saved, created, e := model.ReserveMigrationProbeTask(ctx, f.store.DB, h, task)
	require.NoError(t, e)
	require.True(t, created)
	require.NotEmpty(t, saved.AdmissionGeneration)
	var run model.ChannelMigrationProbeRun
	require.NoError(t, f.store.DB.First(&run).Error)
	require.Equal(t, saved.AdmissionGeneration, run.UseID)
	_, _, e = model.ReserveMigrationProbeTask(ctx, f.store.DB, replay, task)
	require.Error(t, e)
	require.NoError(t, f.store.DB.Delete(&model.NativeTask{}, "id=?", task.ID).Error)
	_, _, e = model.ReserveMigrationProbeTask(ctx, f.store.DB, replay, task)
	require.Error(t, e)
	var count int64
	require.NoError(t, f.store.DB.Model(&model.NativeTask{}).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionUse{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
}
func TestMigrationProbeAuthorizationCannotDriftOrBypass(t *testing.T) {
	for _, mode := range []string{"input", "budget", "owner", "missing-runtime", "cancel", "task-change", "null-handle"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdmissionFixture(t)
			a, r := probeFixture(t, f)
			ctx := context.Background()
			h, e := f.store.BeginMigrationProbe(ctx, a)
			require.NoError(t, e)
			switch mode {
			case "input":
				a.Input = json.RawMessage(`{"model":"test/native/text-to-video","input":{"prompt":"different","duration":"5"}}`)
				_, e = f.store.BeginMigrationProbe(ctx, a)
			case "budget":
				a.MaxPrepaidMicros = 0
				_, e = f.store.BeginMigrationProbe(ctx, a)
			case "owner":
				a.GroupID = "other"
				_, e = f.store.BeginMigrationProbe(ctx, a)
			case "missing-runtime":
				f.store.LoadDeployment = nil
				_, _, e = model.ReserveMigrationProbeTask(ctx, f.store.DB, h, probeNativeTask(t, h))
			case "cancel":
				_, e = f.store.CancelMigrationShadow(ctx, a.Command.Seal.OperationID, r.RequestHash, r.CheckpointHash)
				require.NoError(t, e)
				_, _, e = model.ReserveMigrationProbeTask(ctx, f.store.DB, h, probeNativeTask(t, h))
			case "task-change":
				task := probeNativeTask(t, h)
				task.Endpoint = "fal-ai/other"
				_, _, e = model.ReserveMigrationProbeTask(ctx, f.store.DB, h, task)
			case "null-handle":
				_, _, e = model.ReserveMigrationProbeTask(ctx, f.store.DB, &model.MigrationProbeHandle{}, probeNativeTask(t, h))
			}
			require.Error(t, e)
			var count int64
			require.NoError(t, f.store.DB.Model(&model.NativeTask{}).Count(&count).Error)
			require.Zero(t, count)
			require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionUse{}).Count(&count).Error)
			require.Zero(t, count)
		})
	}
}

func TestMigrationProbeLostRunCannotAdoptOrRegenerate(t *testing.T) {
	f := newAdmissionFixture(t)
	a, _ := probeFixture(t, f)
	h, e := f.store.BeginMigrationProbe(t.Context(), a)
	require.NoError(t, e)
	task := probeNativeTask(t, h)
	_, _, e = model.ReserveMigrationProbeTask(t.Context(), f.store.DB, h, task)
	require.NoError(t, e)
	require.NoError(t, f.store.DB.Exec("DELETE FROM channel_migration_probe_runs").Error)
	read := f.store.ReadMigrationProbe(t.Context(), a.Command.Seal.OperationID)
	require.Equal(t, "unknown", read.State)
	require.NoError(t, f.store.DB.Exec("DELETE FROM native_tasks").Error)
	_, e = f.store.BeginMigrationProbe(t.Context(), a)
	require.Error(t, e)
	require.NoError(t, f.store.DB.Create(&model.Group{ID: "other-internal", Status: model.GroupStatusInternal}).Error)
	require.NoError(t, f.store.DB.Create(&model.Token{ID: 2, GroupID: "other-internal", Status: model.TokenStatusEnabled, Name: "other"}).Error)
	a.GroupID = "other-internal"
	a.TokenID = 2
	_, e = f.store.BeginMigrationProbe(t.Context(), a)
	require.Error(t, e)
	var n int64
	require.NoError(t, f.store.DB.Model(&model.NativeTask{}).Count(&n).Error)
	require.Zero(t, n)
	require.NoError(t, f.store.DB.Model(&model.ChannelMigrationProbeRun{}).Count(&n).Error)
	require.Zero(t, n)
}

// Draft execution is limited to the isolated fixture. It proves the hand-reviewed
// DDL carries the exact columns the command queries, without any runtime installer.
func replayProbeDraft(t *testing.T, f *admissionFixture) {
	t.Helper()
	db := f.store.DB
	require.NoError(t, db.Migrator().DropTable(&model.ChannelMigrationProbeRun{}, &model.ChannelMigrationProbeRuntimeProtocol{}))
	raw, e := os.ReadFile("../../docs/sql/b2-migration-probe-runtime-" + db.Dialector.Name() + "-draft.sql")
	require.NoError(t, e)
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines[i] = ""
		}
	}
	for _, statement := range strings.Split(strings.Join(lines, "\n"), ";") {
		if strings.TrimSpace(statement) != "" {
			require.NoError(t, db.Exec(statement).Error)
		}
	}
}
func TestMigrationProbeDraftAndPartialStorage(t *testing.T) {
	for _, mode := range []string{"draft", "missing", "partial", "wrong-version"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdmissionFixture(t)
			a, r := probeFixture(t, f)
			replayProbeDraft(t, f)
			switch mode {
			case "missing":
				require.NoError(t, f.store.DB.Migrator().DropTable(&model.ChannelMigrationProbeRun{}, &model.ChannelMigrationProbeRuntimeProtocol{}))
			case "partial":
				require.NoError(t, f.store.DB.Exec("ALTER TABLE channel_migration_probe_runs DROP COLUMN owner_hash").Error)
				require.False(t, f.store.DB.Migrator().HasColumn(&model.ChannelMigrationProbeRun{}, "owner_hash"))
			case "wrong-version":
				require.NoError(t, f.store.DB.Model(&model.ChannelMigrationProbeRuntimeProtocol{}).Where("id=1").Update("version", 99).Error)
			}
			h, e := f.store.BeginMigrationProbe(t.Context(), a)
			if mode == "draft" {
				require.NoError(t, e)
				_, _, e = model.ReserveMigrationProbeTask(t.Context(), f.store.DB, h, probeNativeTask(t, h))
				require.NoError(t, e)
			} else {
				require.Error(t, e)
				read := f.store.ReadMigrationProbe(t.Context(), a.Command.Seal.OperationID)
				require.NotEqual(t, "absent", read.State)
				// This optional new extension does not disable ordinary admission on an
				// existing enabled channel when the rest of its original protocol is valid.
				_, _, e = f.store.Admit(t.Context(), admissionRequest("ordinary-existing", r.From), nil)
				require.NoError(t, e)
			}
		})
	}
}
