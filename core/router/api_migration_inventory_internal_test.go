package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestMigrationInventoryHTTPAuthAndCompleteResponse(t *testing.T) {
	db, e := model.OpenSQLite(filepath.Join(t.TempDir(), "http.db"))
	require.NoError(t, e)

	pool, _ := db.DB()
	defer pool.Close()

	oldDB, oldLog := model.DB, model.LogDB
	model.DB, model.LogDB = db, db
	t.Cleanup(func() { model.DB, model.LogDB = oldDB, oldLog })
	r := nativeValidationRouter(t)
	request := func(path, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}

		out := httptest.NewRecorder()
		r.ServeHTTP(out, req)
		require.Equal(t, "no-store", out.Header().Get("Cache-Control"))

		return out
	}
	path := "/api/channel-migrations/inventory"
	require.Equal(t, 401, request(path, "").Code)
	require.Equal(t, 401, request(path, "wrong").Code)
	require.Equal(t, 401, request(path+"?key="+config.AdminKey, "").Code)
	require.Equal(t, 401, request(path+"?key=ignored", config.AdminKey).Code)
	out := request(path, config.AdminKey)
	require.Equal(t, 200, out.Code)
	require.Contains(t, out.Body.String(), `"state":"legacy"`)
	require.NoError(
		t,
		db.AutoMigrate(
			&model.Channel{},
			&model.ModelConfig{},
			&model.ChannelAdmissionControl{},
			&model.ChannelAdmissionProof{},
			&model.ChannelAdmissionFamily{},
			&model.ChannelAdmissionBarrier{},
			&model.ChannelAdmissionUse{},
			&model.ChannelMigrationProtocol{},
			&model.ChannelMigrationPrepared{},
			&model.ChannelMigrationProbeRecord{},
		),
	)
	require.NoError(
		t,
		db.Create(
			&model.ChannelAdmissionControl{
				ID:                1,
				Version:           1,
				Epoch:             "synthetic-epoch",
				Revision:          7,
				EpochSequence:     1,
				StorageGeneration: "synthetic-storage",
				ActiveProofID:     "synthetic-proof",
			},
		).Error,
	)
	require.NoError(t, db.Create(&model.ChannelMigrationProtocol{ID: 1, Version: 1}).Error)
	// This channel's credential must never appear in the authenticated JSON.
	require.NoError(
		t,
		db.Create(
			&model.Channel{
				ID:      1,
				Name:    "example",
				Key:     "synthetic-inventory-credential",
				Type:    model.ChannelTypeFal,
				BaseURL: "https://example.invalid",
			},
		).Error,
	)
	require.NoError(
		t,
		db.Exec(
			"INSERT INTO model_configs (model,config,plugin) VALUES (?,?,?)",
			"example",
			`{"n":900719925474099312345}`,
			`{"cachefollow":{"enable":true}}`,
		).Error,
	)

	out = request(path, config.AdminKey)
	require.Equal(t, 200, out.Code)

	var wire struct {
		Success bool                     `json:"success"`
		Data    model.MigrationInventory `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Body.Bytes(), &wire))
	require.True(t, wire.Success)
	require.True(t, wire.Data.Complete)
	require.Equal(t, "ready", wire.Data.State)
	require.Equal(t, "7", wire.Data.Revision)
	require.Len(t, wire.Data.Channels, 1)
	require.Equal(t, []string{"example"}, wire.Data.ConfigModels)
	require.Contains(t, wire.Data.Features, "registry_provider_fal_async_v1")
	require.NotContains(t, out.Body.String(), "synthetic-inventory-credential")
	require.NoError(
		t,
		db.Exec("UPDATE model_configs SET plugin=?", `{"api_key":"synthetic-plugin-secret"}`).Error,
	)

	out = request(path, config.AdminKey)
	require.NotContains(t, out.Body.String(), "synthetic-plugin-secret")
	require.Contains(t, out.Body.String(), `"state":"unsupported"`)
	require.Contains(t, out.Body.String(), `"complete":false`)
	require.NoError(t, db.Migrator().DropTable(&model.ChannelMigrationProbeRecord{}))

	out = request(path, config.AdminKey)
	require.Contains(t, out.Body.String(), `"state":"unsupported"`)
}
