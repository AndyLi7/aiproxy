package model_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

const shadowTestKey = "synthetic-shadow-resource-key"

func shadowFixture(t *testing.T, f *admissionFixture) model.MigrationShadowRequest {
	inventoryFixture(t, f)
	require.NoError(t, f.store.DB.Exec("UPDATE channels SET status=1 WHERE id=3").Error)
	require.NoError(t, f.store.DB.AutoMigrate(&model.ChannelMigrationShadowProtocol{}, &model.ChannelMigrationShadowResource{}))
	require.NoError(t, f.store.DB.Create(&model.ChannelMigrationShadowProtocol{ID: 1, Version: 2}).Error)
	if f.store.DB.Dialector.Name() == "postgres" {
		require.NoError(t, f.store.DB.Exec("SELECT setval(pg_get_serial_sequence('channels','id'), (SELECT max(id) FROM channels))").Error)
	}
	inv := f.store.ReadMigrationInventory(context.Background(), nil)
	require.Equal(t, "ready", inv.State)
	sum := sha256.Sum256([]byte(shadowTestKey))
	return model.MigrationShadowRequest{Version: 1, OperationID: "shadow-resource", Source: model.MigrationShadowSource{SourceSnapshotHash: inv.SourceSnapshotHash, Epoch: inv.Epoch, Revision: inv.Revision, StorageGeneration: inv.StorageGeneration, From: inv.Channels[0].Identity}, Target: model.MigrationShadowTarget{CredentialID: "new-credential", CredentialDigest: hex.EncodeToString(sum[:]), Projection: model.MigrationShadowProjection{Name: "own-shadow", Type: 59, BaseURL: "https://queue.fal.run", Models: []string{"example"}, ModelMapping: map[string]string{"example": "fal/fixture"}, Sets: []string{"default"}, Priority: 1}}}
}
func countShadows(t *testing.T, f *admissionFixture) int64 {
	var n int64
	require.NoError(t, f.store.DB.Model(&model.ChannelMigrationShadowResource{}).Count(&n).Error)
	return n
}
func TestMigrationShadowAtomicReplayAndCheckpoint(t *testing.T) {
	f := newAdmissionFixture(t)
	req := shadowFixture(t, f)
	ctx := context.Background()
	before := migrationConfig(t, f.store.DB)
	absent := f.store.ReadMigrationShadow(ctx, req.OperationID, nil)
	require.Equal(t, "absent", absent.State)
	got := f.store.CreateMigrationShadow(ctx, req, shadowTestKey, nil)
	require.Equal(t, "ready", got.State)
	require.NotNil(t, got.Receipt)
	require.Equal(t, model.ChannelStatusDisabled, migrationStatus(t, f.store.DB, got.Receipt.Shadow.ChannelID))
	require.Equal(t, int64(1), countShadows(t, f))
	require.Equal(t, before, migrationConfig(t, f.store.DB))
	require.Equal(t, got, f.store.CreateMigrationShadow(ctx, req, shadowTestKey, nil))
	require.Equal(t, got, f.store.ReadMigrationShadow(ctx, req.OperationID, nil))
	// Restart has no process memory ownership dependency.
	restarted := &model.ChannelAdmissionStore{DB: f.store.DB, LogDB: f.store.DB}
	require.Equal(t, got, restarted.ReadMigrationShadow(ctx, req.OperationID, nil))
	_, _, err := f.store.Admit(ctx, admissionRequest("revision-only", f.identities[4]), nil)
	require.NoError(t, err)
	_, err = f.store.Seal(ctx, f.seal("unrelated-family", 1, 5, 3, ""))
	require.NoError(t, err)
	later := f.store.ReadMigrationShadow(ctx, req.OperationID, nil)
	require.Equal(t, "ready", later.State)
	require.Equal(t, got.Receipt, later.Receipt)
	require.NotEqual(t, got.CurrentRevision, later.CurrentRevision)
	changed := req
	changed.Target.CredentialID = "different"
	require.Equal(t, "conflict", f.store.CreateMigrationShadow(ctx, changed, shadowTestKey, nil).State)
	changed = req
	changed.OperationID = "second-operation"
	require.Equal(t, "conflict", f.store.CreateMigrationShadow(ctx, changed, shadowTestKey, nil).State)
	require.Equal(t, int64(1), countShadows(t, f))
	raw, err := json.Marshal(later)
	require.NoError(t, err)
	require.NotContains(t, string(raw), shadowTestKey)
	var row model.ChannelMigrationShadowResource
	require.NoError(t, f.store.DB.First(&row).Error)
	require.NotContains(t, row.RequestJSON, shadowTestKey)
	_, err = f.store.Seal(ctx, f.seal("related-family", 2, 1, 2, ""))
	require.NoError(t, err)
	require.Equal(t, "conflict", f.store.ReadMigrationShadow(ctx, req.OperationID, nil).State)
}
func TestMigrationShadowSourceChangesRefuseBeforeInsert(t *testing.T) {
	for _, change := range []string{"from-status", "from-key", "member-add", "config-change", "config-add", "config-delete", "epoch", "revision", "features", "unsupported"} {
		t.Run(change, func(t *testing.T) {
			f := newAdmissionFixture(t)
			req := shadowFixture(t, f)
			db := f.store.DB
			features := []string(nil)
			switch change {
			case "from-status":
				require.NoError(t, db.Exec("UPDATE channels SET status=2 WHERE id=1").Error)
			case "from-key":
				require.NoError(t, db.Exec("UPDATE channels SET key='synthetic-replaced' WHERE id=1").Error)
			case "member-add":
				require.NoError(t, db.Create(&model.Channel{Name: "unrelated", Status: 2}).Error)
			case "config-change":
				require.NoError(t, db.Exec("UPDATE model_configs SET rpm=99").Error)
			case "config-add":
				require.NoError(t, db.Create(&model.ModelConfig{Model: "another"}).Error)
			case "config-delete":
				require.NoError(t, db.Exec("DELETE FROM model_configs").Error)
			case "epoch":
				require.NoError(t, db.Exec("UPDATE channel_admission_controls SET epoch='new'").Error)
			case "revision":
				require.NoError(t, db.Exec("UPDATE channel_admission_controls SET revision=revision+1").Error)
			case "features":
				features = []string{"different"}
			case "unsupported":
				require.NoError(t, db.Exec("UPDATE channels SET configs=? WHERE id=1", `{"authentication_token":"private-independent"}`).Error)
			}
			var before int64
			require.NoError(t, db.Unscoped().Model(&model.Channel{}).Count(&before).Error)
			got := f.store.CreateMigrationShadow(context.Background(), req, shadowTestKey, features)
			require.NotEqual(t, "ready", got.State)
			require.Zero(t, countShadows(t, f))
			var after int64
			require.NoError(t, db.Unscoped().Model(&model.Channel{}).Count(&after).Error)
			require.Equal(t, before, after)
		})
	}
}
func TestMigrationShadowCorruptOrEditedNeverAbsent(t *testing.T) {
	for _, change := range []string{"request", "request-duplicate", "checkpoint", "identity", "shadow-edit", "shadow-delete", "member-add", "config-change", "epoch", "revision-rewind", "resource-delete-effects"} {
		t.Run(change, func(t *testing.T) {
			f := newAdmissionFixture(t)
			req := shadowFixture(t, f)
			ctx := context.Background()
			got := f.store.CreateMigrationShadow(ctx, req, shadowTestKey, nil)
			require.Equal(t, "ready", got.State)
			db := f.store.DB
			switch change {
			case "request":
				require.NoError(t, db.Exec("UPDATE channel_migration_shadow_resources SET request_hash=?", strings.Repeat("a", 64)).Error)
			case "request-duplicate":
				require.NoError(t, db.Exec("UPDATE channel_migration_shadow_resources SET request_json=?", `{"version":1,"version":1}`).Error)
			case "checkpoint":
				require.NoError(t, db.Exec("UPDATE channel_migration_shadow_resources SET checkpoint_hash=?", strings.Repeat("b", 64)).Error)
			case "identity":
				require.NoError(t, db.Exec("UPDATE channel_migration_shadow_resources SET shadow_identity_hash=?", strings.Repeat("c", 64)).Error)
			case "shadow-edit":
				require.NoError(t, db.Exec("UPDATE channels SET priority=9 WHERE id=?", got.Receipt.Shadow.ChannelID).Error)
			case "shadow-delete":
				require.NoError(t, db.Exec("DELETE FROM channels WHERE id=?", got.Receipt.Shadow.ChannelID).Error)
			case "member-add":
				require.NoError(t, db.Create(&model.Channel{Name: "later", Status: 2}).Error)
			case "config-change":
				require.NoError(t, db.Exec("UPDATE model_configs SET rpm=3").Error)
			case "epoch":
				require.NoError(t, db.Exec("UPDATE channel_admission_controls SET epoch='new'").Error)
			case "revision-rewind":
				require.NoError(t, db.Exec("UPDATE channel_admission_controls SET revision=0").Error)
			case "resource-delete-effects":
				require.NoError(t, db.Exec("DELETE FROM channel_migration_shadow_resources").Error)
				require.NoError(t, db.Create(&model.ChannelMigrationPrepared{OperationID: req.OperationID, State: "prepared"}).Error)
			}
			read := f.store.ReadMigrationShadow(ctx, req.OperationID, nil)
			require.NotEqual(t, "ready", read.State)
			require.NotEqual(t, "absent", read.State)
			require.Nil(t, read.Receipt)
		})
	}
}
func TestMigrationShadowRollbackAndNumericTarget(t *testing.T) {
	t.Run("resource-insert-fails", func(t *testing.T) {
		f := newAdmissionFixture(t)
		req := shadowFixture(t, f)
		require.NoError(t, f.store.DB.Exec(`CREATE TRIGGER no_resource BEFORE INSERT ON channel_migration_shadow_resources BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`).Error)
		got := f.store.CreateMigrationShadow(context.Background(), req, shadowTestKey, nil)
		require.NotEqual(t, "ready", got.State)
		require.Zero(t, countShadows(t, f))
		var n int64
		require.NoError(t, f.store.DB.Model(&model.Channel{}).Count(&n).Error)
		require.Equal(t, int64(5), n)
		require.Equal(t, req.Source.Revision, f.store.ReadMigrationInventory(context.Background(), nil).Revision)
	})
	t.Run("large-number-raw-preserved", func(t *testing.T) {
		f := newAdmissionFixture(t)
		req := shadowFixture(t, f)
		raw := `{"unknown":{"number":900719925474099312345678901,"decimal":1.23000000000000000000009}}`
		req.Target.Projection.ConfigsJSON = &raw
		got := f.store.CreateMigrationShadow(context.Background(), req, shadowTestKey, nil)
		require.Equal(t, "ready", got.State)
		var stored string
		require.NoError(t, f.store.DB.Table("channels").Select("configs").Where("id=?", got.Receipt.Shadow.ChannelID).Scan(&stored).Error)
		require.Contains(t, stored, "900719925474099312345678901")
		require.Contains(t, stored, "1.23000000000000000000009")
		require.Equal(t, got, f.store.ReadMigrationShadow(context.Background(), req.OperationID, nil))
	})
	for _, value := range []string{`{"x":1,"x":2}`, `{"customCredential":"private"}`, `{"outbound_policy":"Bearer private"}`} {
		t.Run(fmt.Sprintf("reject-%s", value), func(t *testing.T) {
			f := newAdmissionFixture(t)
			req := shadowFixture(t, f)
			req.Target.Projection.ConfigsJSON = &value
			require.NotEqual(t, "ready", f.store.CreateMigrationShadow(context.Background(), req, shadowTestKey, nil).State)
			require.Zero(t, countShadows(t, f))
		})
	}
}

func TestMigrationShadowDeletedReceiptDoesNotAuthorizeRecreate(t *testing.T) {
	f := newAdmissionFixture(t)
	req := shadowFixture(t, f)
	ctx := context.Background()
	got := f.store.CreateMigrationShadow(ctx, req, shadowTestKey, nil)
	require.Equal(t, "ready", got.State)
	require.NoError(t, f.store.DB.Exec("DELETE FROM channel_migration_shadow_resources").Error)
	// Absence describes this complete database snapshot, not recovery history.
	require.Equal(t, "absent", f.store.ReadMigrationShadow(ctx, req.OperationID, nil).State)
	require.Equal(t, "conflict", f.store.CreateMigrationShadow(ctx, req, shadowTestKey, nil).State)
	var n int64
	require.NoError(t, f.store.DB.Model(&model.Channel{}).Count(&n).Error)
	require.Equal(t, int64(6), n)
}

func TestMigrationShadowActualPublisherSourcePreserved(t *testing.T) {
	for _, kind := range []string{"ordinary", "video", "image", "tts"} {
		t.Run(kind, func(t *testing.T) {
			f := newAdmissionFixture(t)
			req := shadowFixture(t, f)
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
			require.NoError(t, f.store.DB.Exec("INSERT INTO model_configs(model,owner,type,config,plugin) VALUES(?,?,?,?,?)", publication.Model, publication.Config.Owner, publication.Config.Type, string(publication.Config.Config), "{}").Error)
			inv := f.store.ReadMigrationInventory(context.Background(), nil)
			require.Equal(t, "ready", inv.State)
			req.Source.SourceSnapshotHash = inv.SourceSnapshotHash
			req.Target.Projection.Models = []string{publication.Model}
			req.Target.Projection.ModelMapping = map[string]string{publication.Model: "fal/fixture"}
			got := f.store.CreateMigrationShadow(context.Background(), req, shadowTestKey, nil)
			require.Equal(t, "ready", got.State)
			var after string
			require.NoError(t, f.store.DB.Table("model_configs").Select("config").Where("model=?", publication.Model).Scan(&after).Error)
			require.Equal(t, string(publication.Config.Config), after)
			require.Equal(t, got, f.store.ReadMigrationShadow(context.Background(), req.OperationID, nil))
		})
	}
}

func TestMigrationShadowPersistedTargetCannotDrift(t *testing.T) {
	for _, change := range []string{"name='changed-by-trigger'", `configs='{"unknown":123}'`} {
		t.Run(change, func(t *testing.T) {
			f := newAdmissionFixture(t)
			req := shadowFixture(t, f)
			require.NoError(t, f.store.DB.Exec("CREATE TRIGGER alter_shadow AFTER INSERT ON channels BEGIN UPDATE channels SET "+change+" WHERE id=NEW.id; END").Error)
			require.Equal(t, "conflict", f.store.CreateMigrationShadow(context.Background(), req, shadowTestKey, nil).State)
			require.Zero(t, countShadows(t, f))
			var n int64
			require.NoError(t, f.store.DB.Model(&model.Channel{}).Count(&n).Error)
			require.Equal(t, int64(5), n)
		})
	}
}

func TestMigrationShadowSQLDraft(t *testing.T) {
	f := newAdmissionFixture(t)
	req := shadowFixture(t, f)
	db := f.store.DB
	require.NoError(t, db.Migrator().DropTable(&model.ChannelMigrationShadowResource{}, &model.ChannelMigrationShadowProtocol{}))
	raw, e := os.ReadFile("../../docs/sql/b2-shadow-resources-sqlite-draft.sql")
	require.NoError(t, e)
	require.NoError(t, db.Exec(string(raw)).Error)
	require.Equal(t, "ready", f.store.CreateMigrationShadow(context.Background(), req, shadowTestKey, nil).State)
}
