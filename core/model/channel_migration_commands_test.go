package model_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"os"
	"strings"
	"testing"
	"time"
)

func migrationFixture(t *testing.T, f *admissionFixture, to int) (model.ChannelMigrationRequest, *model.VerifiedMigrationProbe) {
	t.Helper()
	db := f.store.DB
	require.NoError(t, db.AutoMigrate(&model.ModelConfig{}, &model.ChannelMigrationProtocol{}, &model.ChannelMigrationPrepared{}, &model.ChannelMigrationProbeRecord{}))
	require.NoError(t, db.Create(&model.ChannelMigrationProtocol{ID: 1, Version: 1}).Error)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", to).Update("status", model.ChannelStatusDisabled).Error)
	raw := fmt.Sprintf(`{"unknown":{"large":9007199254740993123456789,"array":[1,{"extra":"keep"}]},"upstream_prepayment_v1":{"version":1,"quoteVersion":"frozen","unknown":{"decimal":1.230000000000000000009},"routes":[{"channelId":1,"routeId":"r1","endpoint":"fal/example","credentialScope":"old","keyFingerprint":%q,"quote":{"amount":123456789012345678901,"encryptionVersion":7}}]},"x_token_platform_native_result_v1":{"version":1,"channelId":1,"endpoint":"fal/example","credentialScope":"old","keyFingerprint":%q,"extra":{"revision":987654321098765432109}}}`, f.identities[0].KeyDigest, f.identities[0].KeyDigest)
	require.NoError(t, db.Exec("INSERT INTO model_configs (model,config,plugin) VALUES (?,?,?)", "example", raw, `{"unknown":{"large":123456789012345678901}}`).Error)
	snap, err := f.store.ReadMigrationSnapshot(context.Background(), f.identities[0], f.identities[to-1])
	require.NoError(t, err)
	req := model.ChannelMigrationRequest{Seal: f.seal("prepared", 0, 1, to, ""), FromCredentialID: "old", ToCredentialID: "new", ExpectedPinHash: snap.PinHash, ProjectionHash: snap.ProjectionHash, FamilyHash: snap.FamilyHash}
	if to == 2 {
		req.ToCredentialID = "old"
	}
	rawReq, err := json.Marshal(req)
	require.NoError(t, err)
	sum := sha256.Sum256(rawReq)
	probe := &model.VerifiedMigrationProbe{RequestHash: hex.EncodeToString(sum[:]), EvidenceHash: strings.Repeat("a", 64), StorageGeneration: f.proof.StorageGeneration, IssuedAtMs: time.Now().Add(-time.Minute).UnixMilli(), ValidUntilMs: time.Now().Add(time.Minute).UnixMilli()}
	hash, err := probe.Fingerprint()
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.ChannelMigrationProbeRecord{EvidenceHash: probe.EvidenceHash, FactHash: hash}).Error)
	f.store.LoadMigrationProbe = func(context.Context, model.ChannelMigrationRequest) (model.VerifiedMigrationProbe, error) {
		return *probe, nil
	}
	return req, probe
}
func migrationConfig(t *testing.T, db *gorm.DB) string {
	var raw string
	require.NoError(t, db.Table("model_configs").Select("config").Where("model = ?", "example").Scan(&raw).Error)
	return raw
}
func migrationStatus(t *testing.T, db *gorm.DB, id int) int {
	var ch model.Channel
	require.NoError(t, db.First(&ch, id).Error)
	return ch.Status
}
func TestPreparedMigrationAtomicPinsAndEvidence(t *testing.T) {
	for _, to := range []int{2, 3} {
		t.Run(fmt.Sprint(to), func(t *testing.T) {
			f := newAdmissionFixture(t)
			req, _ := migrationFixture(t, f, to)
			ctx := context.Background()
			before := migrationConfig(t, f.store.DB)
			prepared, err := f.store.PrepareMigration(ctx, req)
			require.NoError(t, err)
			require.Equal(t, "prepared", prepared.State)
			replay, err := f.store.PrepareMigration(ctx, req)
			require.NoError(t, err)
			require.Equal(t, prepared, replay)
			// Unrelated admission advances control; reviewed family identities remain valid.
			_, _, err = f.store.Admit(ctx, admissionRequest("unrelated", f.identities[4]), nil)
			require.NoError(t, err)
			seal, err := f.store.SealPrepared(ctx, req)
			require.NoError(t, err)
			require.Equal(t, model.ChannelStatusDisabled, migrationStatus(t, f.store.DB, to))
			require.Equal(t, before, migrationConfig(t, f.store.DB))
			again, err := f.store.SealPrepared(ctx, req)
			require.NoError(t, err)
			require.Equal(t, seal, again)
			result, err := f.store.CommitPreparedPins(ctx, req, seal.Revision)
			require.NoError(t, err)
			require.Equal(t, "committed", result.State)
			require.Equal(t, model.ChannelStatusEnabled, migrationStatus(t, f.store.DB, to))
			raw := migrationConfig(t, f.store.DB)
			require.Contains(t, raw, "9007199254740993123456789")
			require.Contains(t, raw, "1.230000000000000000009")
			require.Contains(t, raw, "123456789012345678901")
			require.Contains(t, raw, "987654321098765432109")
			require.Contains(t, raw, `"encryptionVersion":7`)
			var cfg map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(raw), &cfg))
			var q struct {
				Routes []struct {
					ChannelID int    `json:"channelId"`
					Scope     string `json:"credentialScope"`
				} `json:"routes"`
			}
			require.NoError(t, json.Unmarshal(cfg["upstream_prepayment_v1"], &q))
			require.Equal(t, to, q.Routes[0].ChannelID)
			require.Equal(t, req.ToCredentialID, q.Routes[0].Scope)
			replay, err = f.store.CommitPreparedPins(ctx, req, seal.Revision)
			require.NoError(t, err)
			require.Equal(t, result, replay)
			read, err := f.store.ReadPreparedMigration(ctx, req.Seal.OperationID)
			require.NoError(t, err)
			require.Equal(t, result, read)
			// A later family operation makes the old receipt historical, never current.
			_, err = f.store.Seal(ctx, f.seal("next", result.CommitRevision, to, 5, ""))
			require.NoError(t, err)
			_, err = f.store.ReadPreparedMigration(ctx, req.Seal.OperationID)
			require.Error(t, err)
			_, err = f.store.CommitPreparedPins(ctx, req, seal.Revision)
			require.Error(t, err)
		})
	}
}
func TestPreparedMigrationRejectsChangedFacts(t *testing.T) {
	for _, scenario := range []string{"probe-mismatch", "probe-missing", "probe-expired", "deployment-missing", "projection", "new-config", "config-price", "family", "early-enable", "forged-after-shape", "partial-schema"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAdmissionFixture(t)
			req, probe := migrationFixture(t, f, 3)
			ctx := context.Background()
			switch scenario {
			case "probe-mismatch":
				probe.RequestHash = strings.Repeat("b", 64)
			case "probe-missing":
				f.store.LoadMigrationProbe = nil
			case "probe-expired":
				probe.ValidUntilMs = time.Now().Add(-time.Second).UnixMilli()
			case "deployment-missing":
				f.store.LoadDeployment = nil
			case "partial-schema":
				require.NoError(t, f.store.DB.Migrator().DropTable(&model.ChannelMigrationProtocol{}))
			}
			_, err := f.store.PrepareMigration(ctx, req)
			if strings.HasPrefix(scenario, "probe-") || scenario == "deployment-missing" || scenario == "partial-schema" {
				require.Error(t, err)
				require.Equal(t, model.ChannelStatusDisabled, migrationStatus(t, f.store.DB, 3))
				return
			}
			require.NoError(t, err)
			switch scenario {
			case "projection":
				require.NoError(t, f.store.DB.Model(&model.Channel{}).Where("id = ?", 3).Update("priority", 44).Error)
			case "new-config":
				require.NoError(t, f.store.DB.Exec("INSERT INTO model_configs (model,config) VALUES (?,?)", "new", `{}`).Error)
			case "config-price":
				require.NoError(t, f.store.DB.Table("model_configs").Where("model = ?", "example").Update("plugin", `{"changed":true}`).Error)
			case "family":
				require.NoError(t, f.store.DB.Create(&model.ChannelAdmissionFamily{KeyDigest: req.Seal.From.KeyDigest, AllowedChannelID: 1, AllowedIdentityHash: req.Seal.From.IdentityHash, Revision: 1, OperationID: "foreign"}).Error)
			case "early-enable":
				require.NoError(t, f.store.DB.Model(&model.Channel{}).Where("id = ?", 3).Update("status", 1).Error)
			case "forged-after-shape":
				seal, err := f.store.SealPrepared(ctx, req)
				require.NoError(t, err)
				require.NoError(t, f.store.DB.Model(&model.Channel{}).Where("id = ?", 3).Update("status", 1).Error)
				_, err = f.store.CommitPreparedPins(ctx, req, seal.Revision)
				require.Error(t, err)
				var row model.ChannelMigrationPrepared
				require.NoError(t, f.store.DB.First(&row).Error)
				require.Equal(t, "sealed", row.State)
				return
			}
			_, err = f.store.SealPrepared(ctx, req)
			require.Error(t, err)
			var n int64
			require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionBarrier{}).Count(&n).Error)
			require.Zero(t, n)
		})
	}
}
func TestPreparedMigrationRollbackAndNormalMutationGuards(t *testing.T) {
	f := newAdmissionFixture(t)
	req, _ := migrationFixture(t, f, 3)
	ctx := context.Background()
	before := migrationConfig(t, f.store.DB)
	_, err := f.store.PrepareMigration(ctx, req)
	require.NoError(t, err)
	oldDB, oldLog := model.DB, model.LogDB
	model.DB, model.LogDB = f.store.DB, f.store.DB
	t.Cleanup(func() { model.DB, model.LogDB = oldDB, oldLog })
	require.Error(t, model.UpdateChannelStatusByID(3, model.ChannelStatusEnabled))
	require.Error(t, model.DeleteModelConfig("example"))
	var cfg model.ModelConfig
	require.NoError(t, f.store.DB.Where("model = ?", "example").First(&cfg).Error)
	require.Error(t, model.SaveModelConfig(cfg))
	var raw map[model.ModelConfigKey]any
	require.NoError(t, json.Unmarshal([]byte(before), &raw))
	require.Error(t, model.SaveModelConfig(model.ModelConfig{Model: "new-pin", Config: raw}))
	priority := int32(55)
	ch, err := model.GetChannelByID(3)
	require.NoError(t, err)
	require.Error(t, model.UpdateChannel(ch, &model.ChannelPatch{Priority: &priority}))
	require.NoError(t, model.UpdateChannelStatusByID(5, model.ChannelStatusDisabled))
	// An unrelated config operation remains allowed, but invalidates the full review.
	require.NoError(t, model.SaveModelConfig(model.ModelConfig{Model: "unrelated"}))
	_, err = f.store.SealPrepared(ctx, req)
	require.Error(t, err)
	require.NoError(t, model.DeleteModelConfig("unrelated"))
	seal, err := f.store.SealPrepared(ctx, req)
	require.NoError(t, err)
	name := "fail-migration-receipt"
	require.NoError(t, f.store.DB.Callback().Update().After("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "channel_migration_prepareds" {
			tx.AddError(errors.New("synthetic receipt persistence failure"))
		}
	}))
	_, err = f.store.CommitPreparedPins(ctx, req, seal.Revision)
	require.Error(t, err)
	require.NoError(t, f.store.DB.Callback().Update().Remove(name))
	require.Equal(t, model.ChannelStatusDisabled, migrationStatus(t, f.store.DB, 3))
	require.Equal(t, before, migrationConfig(t, f.store.DB))

}

func TestPreparedMigrationProbeExpiresAfterLastWrite(t *testing.T) {
	f := newAdmissionFixture(t)
	req, probe := migrationFixture(t, f, 3)
	ctx := context.Background()
	probe.ValidUntilMs = time.Now().Add(500 * time.Millisecond).UnixMilli()
	hash, err := probe.Fingerprint()
	require.NoError(t, err)
	require.NoError(t, f.store.DB.Model(&model.ChannelMigrationProbeRecord{}).Where("evidence_hash = ?", probe.EvidenceHash).Update("fact_hash", hash).Error)
	_, err = f.store.PrepareMigration(ctx, req)
	require.NoError(t, err)
	seal, err := f.store.SealPrepared(ctx, req)
	require.NoError(t, err)
	before := migrationConfig(t, f.store.DB)
	reached := false
	name := "expire-probe-after-write"
	require.NoError(t, f.store.DB.Callback().Update().After("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "channel_migration_prepareds" {
			reached = true
			if delay := time.Until(time.UnixMilli(probe.ValidUntilMs).Add(50 * time.Millisecond)); delay > 0 {
				time.Sleep(delay)
			}
		}
	}))
	defer f.store.DB.Callback().Update().Remove(name)
	_, err = f.store.CommitPreparedPins(ctx, req, seal.Revision)
	require.Error(t, err)
	require.True(t, reached)
	require.Equal(t, before, migrationConfig(t, f.store.DB))
	require.Equal(t, model.ChannelStatusDisabled, migrationStatus(t, f.store.DB, 3))
}

func TestPreparedMigrationAbandonRetainsEvidenceAndRejectsLateCommands(t *testing.T) {
	f := newAdmissionFixture(t)
	req, probe := migrationFixture(t, f, 3)
	ctx := context.Background()
	prep, err := f.store.PrepareMigration(ctx, req)
	require.NoError(t, err)
	_, err = f.store.AbandonPrepared(ctx, req.Seal.OperationID, strings.Repeat("f", 64))
	require.Error(t, err)
	abandoned, err := f.store.AbandonPrepared(ctx, req.Seal.OperationID, prep.RequestHash)
	require.NoError(t, err)
	require.Equal(t, "abandoned", abandoned.State)
	again, err := f.store.AbandonPrepared(ctx, req.Seal.OperationID, prep.RequestHash)
	require.NoError(t, err)
	require.Equal(t, abandoned, again)
	_, err = f.store.PrepareMigration(ctx, req)
	require.Error(t, err)
	_, err = f.store.SealPrepared(ctx, req)
	require.Error(t, err)
	_, err = f.store.CommitPreparedPins(ctx, req, 1)
	require.Error(t, err)
	var evidence int64
	require.NoError(t, f.store.DB.Model(&model.ChannelMigrationProbeRecord{}).Count(&evidence).Error)
	require.Equal(t, int64(1), evidence)
	require.Equal(t, model.ChannelStatusDisabled, migrationStatus(t, f.store.DB, 3))
	req.Seal.OperationID = "new-review"
	raw, err := json.Marshal(req)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	probe.RequestHash = hex.EncodeToString(sum[:])
	probe.EvidenceHash = strings.Repeat("b", 64)
	hash, err := probe.Fingerprint()
	require.NoError(t, err)
	require.NoError(t, f.store.DB.Create(&model.ChannelMigrationProbeRecord{EvidenceHash: probe.EvidenceHash, FactHash: hash}).Error)
	prep, err = f.store.PrepareMigration(ctx, req)
	require.NoError(t, err)
	seal, err := f.store.SealPrepared(ctx, req)
	require.NoError(t, err)
	_, err = f.store.AbandonPrepared(ctx, req.Seal.OperationID, prep.RequestHash)
	require.Error(t, err)
	_, err = f.store.CommitPreparedPins(ctx, req, seal.Revision)
	require.NoError(t, err)
	_, err = f.store.AbandonPrepared(ctx, req.Seal.OperationID, prep.RequestHash)
	require.Error(t, err)
}
func TestPreparedMigrationMissingProbeRecordAndRevocation(t *testing.T) {
	for _, scenario := range []string{"missing", "revoked", "changed-fact", "wrong-version", "missing-column", "split-storage"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAdmissionFixture(t)
			req, probe := migrationFixture(t, f, 3)
			ctx := context.Background()
			switch scenario {
			case "missing":
				require.NoError(t, f.store.DB.Where("evidence_hash = ?", probe.EvidenceHash).Delete(&model.ChannelMigrationProbeRecord{}).Error)
			case "revoked":
				require.NoError(t, f.store.DB.Model(&model.ChannelMigrationProbeRecord{}).Where("evidence_hash = ?", probe.EvidenceHash).Update("revoked", true).Error)
			case "changed-fact":
				require.NoError(t, f.store.DB.Model(&model.ChannelMigrationProbeRecord{}).Where("evidence_hash = ?", probe.EvidenceHash).Update("fact_hash", strings.Repeat("f", 64)).Error)
			case "wrong-version":
				require.NoError(t, f.store.DB.Model(&model.ChannelMigrationProtocol{}).Where("id = ?", 1).Update("version", 2).Error)
			case "missing-column":
				require.NoError(t, f.store.DB.Migrator().DropColumn(&model.ChannelMigrationPrepared{}, "probe_fact_hash"))
			case "split-storage":
				f.store.LogDB = f.store.DB.Session(&gorm.Session{})
			}
			_, err := f.store.PrepareMigration(ctx, req)
			require.Error(t, err)
			var n int64
			require.NoError(t, f.store.DB.Model(&model.ChannelMigrationPrepared{}).Count(&n).Error)
			require.Zero(t, n)
		})
	}
}
func TestPreparedMigrationRejectsAmbiguousJSONWithoutPartialWrites(t *testing.T) {
	f := newAdmissionFixture(t)
	req, probe := migrationFixture(t, f, 3)
	ctx := context.Background()
	raw := migrationConfig(t, f.store.DB)
	raw = strings.Replace(raw, `"unknown":`, `"duplicate":1,"duplicate":2,"unknown":`, 1)
	require.NoError(t, f.store.DB.Table("model_configs").Where("model = ?", "example").Update("config", raw).Error)
	snap, err := f.store.ReadMigrationSnapshot(ctx, req.Seal.From, req.Seal.To)
	require.NoError(t, err)
	req.ExpectedPinHash = snap.PinHash
	encoded, err := json.Marshal(req)
	require.NoError(t, err)
	sum := sha256.Sum256(encoded)
	probe.RequestHash = hex.EncodeToString(sum[:])
	hash, err := probe.Fingerprint()
	require.NoError(t, err)
	require.NoError(t, f.store.DB.Model(&model.ChannelMigrationProbeRecord{}).Where("evidence_hash = ?", probe.EvidenceHash).Update("fact_hash", hash).Error)
	_, err = f.store.PrepareMigration(ctx, req)
	require.NoError(t, err)
	seal, err := f.store.SealPrepared(ctx, req)
	require.NoError(t, err)
	_, err = f.store.CommitPreparedPins(ctx, req, seal.Revision)
	require.Error(t, err)
	require.Equal(t, raw, migrationConfig(t, f.store.DB))
	require.Equal(t, model.ChannelStatusDisabled, migrationStatus(t, f.store.DB, 3))
}

func TestPreparedMigrationDraftSQLite(t *testing.T) {
	f := newAdmissionFixture(t)
	script, err := os.ReadFile("../migrations/channel-migration-prepared-v1/sqlite.sql")
	require.NoError(t, err)
	for range 2 {
		require.NoError(t, f.store.DB.Exec(string(script)).Error)
	}
	var protocol model.ChannelMigrationProtocol
	require.NoError(t, f.store.DB.First(&protocol, 1).Error)
	require.Equal(t, 1, protocol.Version)
	var n int64
	require.NoError(t, f.store.DB.Model(&model.ChannelMigrationPrepared{}).Count(&n).Error)
	require.Zero(t, n)
	require.NoError(t, f.store.DB.Model(&model.ChannelMigrationProbeRecord{}).Count(&n).Error)
	require.Zero(t, n)
}

func TestPreparedMigrationConfigWritesRejectCorruptStateAndKeepLegacy(t *testing.T) {
	for _, scenario := range []string{"unknown", "empty-state", "empty-records", "missing-extension"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAdmissionFixture(t)
			req, _ := migrationFixture(t, f, 3)
			blocked := scenario == "unknown" || scenario == "empty-state"
			if blocked {
				_, err := f.store.PrepareMigration(context.Background(), req)
				require.NoError(t, err)
				var state any = "corrupt"
				if scenario == "empty-state" {
					state = ""
				}
				require.NoError(t, f.store.DB.Model(&model.ChannelMigrationPrepared{}).Where("operation_id = ?", req.Seal.OperationID).UpdateColumn("state", state).Error)
			} else if scenario == "missing-extension" {
				require.NoError(t, f.store.DB.Migrator().DropTable(&model.ChannelMigrationPrepared{}, &model.ChannelMigrationProtocol{}, &model.ChannelMigrationProbeRecord{}))
			}
			oldDB, oldLog := model.DB, model.LogDB
			model.DB, model.LogDB = f.store.DB, f.store.DB
			t.Cleanup(func() { model.DB, model.LogDB = oldDB, oldLog })
			before := migrationConfig(t, f.store.DB)
			calls := []func() error{
				func() error { return model.SaveModelConfig(model.ModelConfig{Model: "new-unrelated"}) },
				func() error { return model.SaveModelConfigs([]model.ModelConfig{{Model: "new-batch"}}) },
				func() error { return model.SaveModelConfig(model.ModelConfig{Model: "example"}) },
				func() error { return model.DeleteModelConfig("example") },
				func() error { return model.DeleteModelConfigsByModels([]string{"example"}) },
			}
			for _, call := range calls {
				err := call()
				if blocked {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
			}
			if blocked {
				require.Equal(t, before, migrationConfig(t, f.store.DB))
				var n int64
				require.NoError(t, f.store.DB.Model(&model.ModelConfig{}).Where("model IN ?", []string{"new-unrelated", "new-batch"}).Count(&n).Error)
				require.Zero(t, n)
			}
		})
	}
}

func TestPreparedMigrationReadRejectsNewControlEpoch(t *testing.T) {
	f := newAdmissionFixture(t)
	req, _ := migrationFixture(t, f, 3)
	_, err := f.store.PrepareMigration(context.Background(), req)
	require.NoError(t, err)
	_, err = f.store.SealPrepared(context.Background(), req)
	require.NoError(t, err)
	require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionControl{}).Where("id = ?", 1).UpdateColumn("epoch", "new-epoch").Error)
	_, err = f.store.ReadPreparedMigration(context.Background(), req.Seal.OperationID)
	require.Error(t, err)
}

// Early reversal targets the still-enabled original channel. This command set
// deliberately refuses that topology; callers must not disable it to fake a shadow.
func TestPreparedMigrationDoesNotPretendToImplementLiveReverse(t *testing.T) {
	f := newAdmissionFixture(t)
	req, probe := migrationFixture(t, f, 3)
	ctx := context.Background()
	_, err := f.store.PrepareMigration(ctx, req)
	require.NoError(t, err)
	seal, err := f.store.SealPrepared(ctx, req)
	require.NoError(t, err)
	committed, err := f.store.CommitPreparedPins(ctx, req, seal.Revision)
	require.NoError(t, err)
	before := migrationConfig(t, f.store.DB)
	snap, err := f.store.ReadMigrationSnapshot(ctx, req.Seal.To, req.Seal.From)
	require.NoError(t, err)
	reverse := model.ChannelMigrationRequest{Seal: f.seal("reverse", committed.CommitRevision, 3, 1, req.Seal.OperationID), FromCredentialID: req.ToCredentialID, ToCredentialID: req.FromCredentialID, ExpectedPinHash: snap.PinHash, ProjectionHash: snap.ProjectionHash, FamilyHash: snap.FamilyHash}
	raw, err := json.Marshal(reverse)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	probe.RequestHash = hex.EncodeToString(sum[:])
	probe.EvidenceHash = strings.Repeat("c", 64)
	hash, err := probe.Fingerprint()
	require.NoError(t, err)
	require.NoError(t, f.store.DB.Create(&model.ChannelMigrationProbeRecord{EvidenceHash: probe.EvidenceHash, FactHash: hash}).Error)
	_, err = f.store.PrepareMigration(ctx, reverse)
	require.Error(t, err)
	require.Equal(t, model.ChannelStatusEnabled, migrationStatus(t, f.store.DB, 1))
	require.Equal(t, model.ChannelStatusEnabled, migrationStatus(t, f.store.DB, 3))
	require.Equal(t, before, migrationConfig(t, f.store.DB))
	var n int64
	require.NoError(t, f.store.DB.Model(&model.ChannelMigrationPrepared{}).Where("operation_id = ?", "reverse").Count(&n).Error)
	require.Zero(t, n)
	// Synthetic fixture precondition only, NOT an exercise or proof of safe-disable.
	require.NoError(t, f.store.DB.Model(&model.Channel{}).Where("id = ?", 1).UpdateColumn("status", model.ChannelStatusDisabled).Error)
	_, err = f.store.PrepareMigration(ctx, reverse)
	require.Error(t, err)
	require.Equal(t, before, migrationConfig(t, f.store.DB))
}
