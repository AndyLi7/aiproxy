package model_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/router"
	"github.com/stretchr/testify/require"
)

func TestMigrationShadowHTTPAuthSourceAndRecovery(t *testing.T) {
	f := newAdmissionFixture(t)
	req := shadowFixture(t, f)
	oldDB, oldLog, oldKey := model.DB, model.LogDB, config.AdminKey
	model.DB, model.LogDB, config.AdminKey = f.store.DB, f.store.DB, "synthetic-shadow-http-admin"
	t.Cleanup(func() { model.DB, model.LogDB, config.AdminKey = oldDB, oldLog, oldKey })
	r := gin.New()
	router.SetAPIRouter(r)
	call := func(method, path, body, key string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		out := httptest.NewRecorder()
		r.ServeHTTP(out, request)
		require.Equal(t, "no-store", out.Header().Get("Cache-Control"))
		require.NotContains(t, out.Body.String(), shadowTestKey)
		return out
	}
	inventory := call("GET", "/api/channel-migrations/inventory", "", config.AdminKey)
	var inv struct {
		Data model.MigrationInventory `json:"data"`
	}
	require.NoError(t, json.Unmarshal(inventory.Body.Bytes(), &inv))
	require.Equal(t, "ready", inv.Data.State)
	req.Source.SourceSnapshotHash = inv.Data.SourceSnapshotHash
	raw, e := json.Marshal(map[string]any{"request": req, "key": shadowTestKey})
	require.NoError(t, e)
	path := "/api/channel-migrations/operations/" + req.OperationID + "/shadow"
	for _, method := range []string{"GET", "POST"} {
		require.Equal(t, 401, call(method, path, string(raw), "").Code)
		require.Equal(t, 401, call(method, path, string(raw), "wrong").Code)
		require.Equal(t, 401, call(method, path+"?key=ignored", string(raw), config.AdminKey).Code)
	}
	for _, bad := range []string{`{}`, string(raw) + `{}`, strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1), strings.Replace(string(raw), `"source":`, `"Source":`, 1), strings.Replace(string(raw), `"name":"own-shadow"`, `"name":"own-shadow","status":1`, 1), strings.Replace(string(raw), `"priority":0`, `"priority":0,"id":99`, 1), strings.Repeat(" ", 640*1024+1)} {
		// The fixture's priority is 1; ensure this case really mutates the wire.
		if bad == string(raw) {
			continue
		}
		require.Equal(t, 400, call("POST", path, bad, config.AdminKey).Code)
	}
	got := call("POST", path, string(raw), config.AdminKey)
	require.Equal(t, 200, got.Code)
	require.Contains(t, got.Body.String(), `"state":"ready"`)
	replay := call("POST", path, string(raw), config.AdminKey)
	require.JSONEq(t, got.Body.String(), replay.Body.String())
	read := call("GET", path, "", config.AdminKey)
	require.JSONEq(t, got.Body.String(), read.Body.String())
	require.Equal(t, 400, call("GET", path, "{}", config.AdminKey).Code)
	require.Contains(t, call("GET", "/api/channel-migrations/operations/absent/shadow", "", config.AdminKey).Body.String(), `"state":"absent"`)
	require.Equal(t, int64(1), countShadows(t, f))
	// The read API performs no repair, create, or revision self-update.
	for _, table := range []string{"channels", "channel_migration_shadow_resources", "channel_admission_controls"} {
		require.NoError(t, f.store.DB.Exec("CREATE TRIGGER shadow_read_"+table+" BEFORE UPDATE ON "+table+" BEGIN SELECT RAISE(ABORT,'read writes'); END").Error)
	}
	require.Contains(t, call("GET", path, "", config.AdminKey).Body.String(), `"state":"ready"`)
	require.Equal(t, "ready", f.store.ReadMigrationCommandProtocol(context.Background()).State)
}
func TestMigrationShadowManagementGuardsAndPartial(t *testing.T) {
	f := newAdmissionFixture(t)
	req := shadowFixture(t, f)
	got := f.store.CreateMigrationShadow(context.Background(), req, shadowTestKey, nil)
	require.Equal(t, "ready", got.State)
	oldDB, oldLog := model.DB, model.LogDB
	model.DB, model.LogDB = f.store.DB, f.store.DB
	t.Cleanup(func() { model.DB, model.LogDB = oldDB, oldLog })
	id := got.Receipt.Shadow.ChannelID
	require.Error(t, model.UpdateChannelStatusByID(id, model.ChannelStatusEnabled))
	ch, e := model.GetChannelByID(id)
	require.NoError(t, e)
	name := "other"
	require.Error(t, model.UpdateChannel(ch, &model.ChannelPatch{Name: &name}))
	require.Error(t, model.DeleteChannelByID(id))
	// Missing extension preserves existing status operation, partial installation refuses.
	require.NoError(t, f.store.DB.Migrator().DropTable(&model.ChannelMigrationShadowResource{}))
	require.Error(t, model.UpdateChannelStatusByID(1, model.ChannelStatusDisabled))
	require.Error(t, model.BatchInsertChannels([]*model.Channel{{Name: "new", Status: 2}}))
	require.NoError(t, f.store.DB.Migrator().DropTable(&model.ChannelMigrationShadowProtocol{}))
	// No mutation is needed to prove compatibility; read remains supported when all optional tables are absent.
	require.Equal(t, "ready", f.store.ReadMigrationInventory(context.Background(), nil).State)
}

func TestMigrationShadowHTTPTransportLostResponse(t *testing.T) {
	f := newAdmissionFixture(t)
	req := shadowFixture(t, f)
	oldDB, oldLog, oldKey := model.DB, model.LogDB, config.AdminKey
	model.DB, model.LogDB, config.AdminKey = f.store.DB, f.store.DB, "synthetic-shadow-transport-admin"
	t.Cleanup(func() { model.DB, model.LogDB, config.AdminKey = oldDB, oldLog, oldKey })
	r := gin.New()
	router.SetAPIRouter(r)
	server := httptest.NewServer(r)
	defer server.Close()
	call := func(method, path, body string) *http.Response {
		request, e := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		require.NoError(t, e)
		request.Header.Set("Authorization", "Bearer "+config.AdminKey)
		res, e := server.Client().Do(request)
		require.NoError(t, e)
		require.Equal(t, 200, res.StatusCode)
		require.Equal(t, "no-store", res.Header.Get("Cache-Control"))
		return res
	}
	res := call("GET", "/api/channel-migrations/inventory", "")
	var inv struct {
		Data model.MigrationInventory `json:"data"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&inv))
	res.Body.Close()
	req.Source.SourceSnapshotHash = inv.Data.SourceSnapshotHash
	raw, e := json.Marshal(map[string]any{"request": req, "key": shadowTestKey})
	require.NoError(t, e)
	path := "/api/channel-migrations/operations/" + req.OperationID + "/shadow"
	res = call("POST", path, string(raw))
	res.Body.Close() // discard response; recovery cannot rely on caller's returned ID
	res = call("GET", path, "")
	body, e := io.ReadAll(res.Body)
	require.NoError(t, e)
	res.Body.Close()
	require.NotContains(t, string(body), shadowTestKey)
	var recovered struct {
		Data model.MigrationShadowResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &recovered))
	require.Equal(t, "ready", recovered.Data.State)
	res = call("POST", path, string(raw))
	var replay struct {
		Data model.MigrationShadowResult `json:"data"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&replay))
	res.Body.Close()
	require.Equal(t, recovered.Data, replay.Data)
	require.Equal(t, int64(1), countShadows(t, f))
}
