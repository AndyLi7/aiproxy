package model_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"strings"
	"testing"
	"time"
)

func inventoryFixture(t *testing.T, f *admissionFixture) (model.ChannelMigrationRequest, *model.VerifiedMigrationProbe) {
	req, probe := migrationFixture(t, f, 3)
	raw := migrationConfig(t, f.store.DB)
	raw = strings.ReplaceAll(raw, `"array":[1,{"extra":"keep"}]`, `"array":[1,{"extra":123}]`)
	require.NoError(t, f.store.DB.Exec("UPDATE model_configs SET config=?,plugin=?", raw, `{"cachefollow":{"enable":true,"followed_channel_ttl_seconds":30}}`).Error)
	snap, e := f.store.ReadMigrationSnapshot(context.Background(), f.identities[0], f.identities[2])
	require.NoError(t, e)
	req.ExpectedPinHash = snap.PinHash
	return req, probe
}
func TestMigrationInventoryCompleteRawReadOnly(t *testing.T) {
	f := newAdmissionFixture(t)
	req, _ := inventoryFixture(t, f)
	db := f.store.DB
	raw := `{"unknown":{"n":900719925474099312345678901,"fraction":1.2300000000000000009}}`
	require.NoError(t, db.Model(&model.Channel{}).Where("id=1").Update("configs", raw).Error)
	require.NoError(t, db.Exec("UPDATE channels SET deleted_at=? WHERE id=5", time.Now().UTC()).Error)
	var original model.Channel
	require.NoError(t, db.First(&original, 1).Error)
	identity, e := model.ChannelIdentityOf(&original)
	require.NoError(t, e)
	var before model.ChannelAdmissionControl
	require.NoError(t, db.First(&before, 1).Error)
	f.store.LoadDeployment = nil
	f.store.LoadMigrationProbe = nil // inventory cannot depend on or invoke either issuer
	got := f.store.ReadMigrationInventory(context.Background(), []string{"z", "a"})
	require.Equal(t, "ready", got.State)
	require.True(t, got.Complete)
	require.Len(t, got.Channels, 5)
	require.Len(t, got.Configs, 1)
	require.Equal(t, req.ExpectedPinHash, got.PinHash)
	require.Equal(t, []string{"example"}, got.ConfigModels)
	require.Equal(t, identity, got.Channels[0].Identity)
	require.Equal(t, raw, *got.Channels[0].IdentityProjection.ConfigsJSON)
	require.NotNil(t, got.Channels[4].Routing.DeletedAt)
	require.Contains(t, got.Configs[0].RawJSON, "123456789012345678901")
	require.Contains(t, got.Configs[0].RawJSON, "1.230000000000000000009")
	encoded, e := json.Marshal(got)
	require.NoError(t, e)
	require.NotContains(t, string(encoded), "synthetic-old")
	require.NotContains(t, string(encoded), "synthetic-new")
	replay := f.store.ReadMigrationInventory(context.Background(), []string{"a", "z"})
	require.Equal(t, got.SourceSnapshotHash, replay.SourceSnapshotHash)
	var after model.ChannelAdmissionControl
	require.NoError(t, db.First(&after, 1).Error)
	require.Equal(t, before, after)
	for _, table := range []any{&model.ChannelAdmissionBarrier{}, &model.ChannelAdmissionFamily{}, &model.ChannelAdmissionUse{}, &model.ChannelMigrationPrepared{}} {
		var n int64
		require.NoError(t, db.Model(table).Count(&n).Error)
		require.Zero(t, n)
	}
	require.NoError(t, db.Model(&model.Channel{}).Where("id=1").Update("name", "changed-safe-name").Error)
	changed := f.store.ReadMigrationInventory(context.Background(), nil)
	require.Equal(t, "ready", changed.State)
	require.NotEqual(t, got.SourceSnapshotHash, changed.SourceSnapshotHash)
}
func TestMigrationInventoryUnsafeRejectsWholeProjection(t *testing.T) {
	cases := []struct {
		name, sql string
		value     any
	}{
		{"alias-authentication-token", "UPDATE model_configs SET plugin=?", `{"cachefollow":{"authentication_token":"private-plugin-token"}}`},
		{"alias-custom-credential", "UPDATE channels SET configs=? WHERE id=1", `{"customCredential":"private-other-secret"}`},
		{"alias-api-key-value", "UPDATE model_configs SET config=?", `{"custom":{"apiKeyValue":"private-other-secret"}}`},
		{"unknown-plugin", "UPDATE model_configs SET plugin=?", `{"unregistered":{"label":"ordinary-looking-but-unproven"}}`},
		{"unknown-plugin-numeric", "UPDATE model_configs SET plugin=?", `{"unregistered":{"enable":true}}`},
		{"deep-config", "UPDATE model_configs SET config=?", `{"unknown":` + strings.Repeat("[", 5000) + "1" + strings.Repeat("]", 5000) + "}"},
		{"deep-channel-before-serializer", "UPDATE channels SET configs=? WHERE id=1", `{"unknown":` + strings.Repeat("[", 5000) + "1" + strings.Repeat("]", 5000) + "}"},
		{"unknown-opaque-column", "ALTER TABLE model_configs ADD COLUMN custom_credential TEXT DEFAULT 'private-other-secret'", nil},
		{"plugin-key", "UPDATE model_configs SET plugin=?", `{"web-search":{"search_from":[{"spec":{"api_key":"do-not-expose"}}]}}`},
		{"plugin-encoded", "UPDATE model_configs SET plugin=?", `{"spec":"{\"authorization\":\"hidden\"}"}`},
		{"configs-key", "UPDATE channels SET configs=? WHERE id=1", `{"apiKey":"do-not-expose"}`},
		{"copied-key", "UPDATE channels SET name=? WHERE id=1", "synthetic-new"},
		{"url-userinfo", "UPDATE channels SET proxy_url=? WHERE id=1", "https://user:password@example.invalid"},
		{"url-query", "UPDATE channels SET base_url=? WHERE id=1", "https://example.invalid/?token=hidden"},
		{"duplicate", "UPDATE model_configs SET config=?", `{"a":1,"a":2}`},
		{"unknown-secret-column", "ALTER TABLE model_configs ADD COLUMN api_key TEXT DEFAULT 'secret-value'", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdmissionFixture(t)
			inventoryFixture(t, f)
			var q *gorm.DB
			if tc.value != nil {
				q = f.store.DB.Exec(tc.sql, tc.value)
			} else {
				q = f.store.DB.Exec(tc.sql)
			}
			require.NoError(t, q.Error)
			got := f.store.ReadMigrationInventory(context.Background(), nil)
			require.Equal(t, "unsupported", got.State)
			require.False(t, got.Complete)
			require.Empty(t, got.Channels)
			require.Empty(t, got.Configs)
			require.Empty(t, got.PinHash)
		})
	}
}
func TestMigrationInventoryUnavailableAndLegacy(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		db, e := model.OpenSQLite(":memory:")
		require.NoError(t, e)
		pool, _ := db.DB()
		defer pool.Close()
		s := &model.ChannelAdmissionStore{DB: db, LogDB: db}
		got := s.ReadMigrationInventory(context.Background(), nil)
		require.Equal(t, "legacy", got.State)
		require.False(t, got.Complete)
	})
	for _, mode := range []string{"partial", "version", "never-active", "split", "revision-large", "unknown-state"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdmissionFixture(t)
			inventoryFixture(t, f)
			db := f.store.DB
			want := "unsupported"
			switch mode {
			case "partial":
				require.NoError(t, db.Migrator().DropTable(&model.ChannelMigrationProbeRecord{}))
			case "version":
				require.NoError(t, db.Model(&model.ChannelMigrationProtocol{}).Where("id=1").Update("version", 2).Error)
			case "never-active":
				require.NoError(t, db.Model(&model.ChannelAdmissionControl{}).Where("id=1").Updates(map[string]any{"epoch": "", "epoch_sequence": 0, "storage_generation": "", "active_proof_id": ""}).Error)
				want = "legacy"
			case "split":
				f.store.LogDB = db.Session(&gorm.Session{})
			case "revision-large":
				require.NoError(t, db.Model(&model.ChannelAdmissionControl{}).Where("id=1").Update("revision", int64(9007199254740993)).Error)
				want = "ready"
			case "unknown-state":
				require.NoError(t, db.Create(&model.ChannelMigrationPrepared{OperationID: "broken", State: "unknown"}).Error)
			}
			got := f.store.ReadMigrationInventory(context.Background(), nil)
			require.Equal(t, want, got.State)
			if mode == "revision-large" {
				require.Equal(t, "9007199254740993", got.Revision)
			}
		})
	}
}
func TestMigrationInventoryBounds(t *testing.T) {
	t.Run("rows", func(t *testing.T) {
		f := newAdmissionFixture(t)
		inventoryFixture(t, f)
		rows := []map[string]any{}
		for i := 0; i < model.MigrationInventoryMaxRows; i++ {
			rows = append(rows, map[string]any{"model": fmt.Sprintf("m%05d", i)})
		}
		require.NoError(t, f.store.DB.Table("model_configs").CreateInBatches(rows, 100).Error)
		got := f.store.ReadMigrationInventory(context.Background(), nil)
		require.Equal(t, "inventory_limit_exceeded", got.ReasonCode)
		require.Empty(t, got.Configs)
	})
	t.Run("bytes", func(t *testing.T) {
		f := newAdmissionFixture(t)
		inventoryFixture(t, f)
		require.NoError(t, f.store.DB.Exec("UPDATE model_configs SET owner=?", strings.Repeat("x", model.MigrationInventoryMaxBytes)).Error)
		got := f.store.ReadMigrationInventory(context.Background(), nil)
		require.Equal(t, "inventory_limit_exceeded", got.ReasonCode)
		require.Empty(t, got.Channels)
	})
}
