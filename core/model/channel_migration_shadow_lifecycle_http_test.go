package model_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/router"
	"github.com/stretchr/testify/require"
)

func lifecycleHTTPRouter(t *testing.T, f *admissionFixture) *gin.Engine {
	t.Helper()
	oldDB, oldLog, oldKey := model.DB, model.LogDB, config.AdminKey
	model.DB, model.LogDB, config.AdminKey = f.store.DB, f.store.DB, "synthetic-lifecycle-admin"
	t.Cleanup(func() { model.DB, model.LogDB, config.AdminKey = oldDB, oldLog, oldKey })
	r := gin.New()
	router.SetAPIRouter(r)
	return r
}
func lifecycleHTTPCall(t *testing.T, r *gin.Engine, method, path, body, key string) (int, model.MigrationShadowLifecycleResult, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	out := httptest.NewRecorder()
	r.ServeHTTP(out, req)
	require.Equal(t, "no-store", out.Header().Get("Cache-Control"))
	require.NotContains(t, out.Body.String(), shadowTestKey)
	require.NotContains(t, out.Body.String(), "synthetic-old")
	var wire struct {
		Data model.MigrationShadowLifecycleResult `json:"data"`
	}
	if out.Code == 200 {
		require.NoError(t, json.Unmarshal(out.Body.Bytes(), &wire))
	}
	return out.Code, wire.Data, out.Body.String()
}
func lifecycleCancelJSON(t *testing.T, r *model.MigrationShadowReceipt) string {
	t.Helper()
	b, e := json.Marshal(map[string]any{"version": 2, "resourceRequestHash": r.RequestHash, "checkpointHash": r.CheckpointHash})
	require.NoError(t, e)
	return string(b)
}
func TestMigrationShadowLifecycleHTTPAuthAndStrictBody(t *testing.T) {
	f := newAdmissionFixture(t)
	source, receipt, _ := lifecycleFixture(t, f)
	r := lifecycleHTTPRouter(t, f)
	base := "/api/channel-migrations/operations/" + source.OperationID + "/shadow"
	body := lifecycleCancelJSON(t, receipt)
	for _, method := range []string{"GET", "POST"} {
		path := base + "/lifecycle"
		if method == "POST" {
			path = base + "/cancel"
		}
		for _, key := range []string{"", "wrong"} {
			code, _, _ := lifecycleHTTPCall(t, r, method, path, body, key)
			require.Equal(t, 401, code)
		}
		code, _, _ := lifecycleHTTPCall(t, r, method, path+"?key=ignored", body, config.AdminKey)
		require.Equal(t, 401, code)
	}
	for _, bad := range []string{`{}`, `null`, body + `{}`, strings.Replace(body, `"version":2`, `"version":2,"version":2`, 1), strings.Replace(body, `"checkpointHash"`, `"CheckpointHash"`, 1), strings.Replace(body, `"version":2`, `"version":null`, 1), strings.Replace(body, `"version":2`, `"version":1`, 1), strings.Replace(body, `"version":2`, `"version":2,"passed":true`, 1), strings.Replace(body, receipt.RequestHash, "bad", 1), strings.Repeat(" ", 4097)} {
		code, _, _ := lifecycleHTTPCall(t, r, "POST", base+"/cancel", bad, config.AdminKey)
		require.Equal(t, 400, code)
	}
	code, _, _ := lifecycleHTTPCall(t, r, "GET", base+"/lifecycle", "{}", config.AdminKey)
	require.Equal(t, 400, code)
	code, _, _ = lifecycleHTTPCall(t, r, "POST", "/api/channel-migrations/operations/-bad/shadow/cancel", body, config.AdminKey)
	require.Equal(t, 400, code)
	require.Equal(t, "created", lifecycleRow(t, f, source.OperationID).LifecycleState)
	code, got, _ := lifecycleHTTPCall(t, r, "GET", base+"/lifecycle", "", config.AdminKey)
	require.Equal(t, 200, code)
	require.Equal(t, "ready", got.State)
	require.Equal(t, 2, got.Version)
	require.Equal(t, source, got.Observation.ResourceRequest)
	require.Equal(t, *receipt, got.Observation.Creation)
	require.Nil(t, got.Observation.PreparedBinding)
	code, got, _ = lifecycleHTTPCall(t, r, "POST", base+"/cancel", body, config.AdminKey)
	require.Equal(t, 200, code)
	require.Equal(t, "cancelled", got.Observation.Lifecycle.State)
	_, replay, _ := lifecycleHTTPCall(t, r, "POST", base+"/cancel", body, config.AdminKey)
	require.Equal(t, got, replay)
	_, read, _ := lifecycleHTTPCall(t, r, "GET", base+"/lifecycle", "", config.AdminKey)
	require.Equal(t, got, read)
	_, absent, _ := lifecycleHTTPCall(t, r, "GET", "/api/channel-migrations/operations/absent/shadow/lifecycle", "", config.AdminKey)
	require.Equal(t, "absent", absent.State)
	require.Nil(t, absent.Observation)
	require.NotEmpty(t, absent.Epoch)
	require.NotEmpty(t, absent.StorageGeneration)
}
func TestMigrationShadowLifecycleHTTPCompleteBindingAndPhases(t *testing.T) {
	for _, phase := range []string{"consumed", "sealed", "committed", "cancelled"} {
		t.Run(phase, func(t *testing.T) {
			f := newAdmissionFixture(t)
			source, receipt, req := lifecycleFixture(t, f)
			ctx := context.Background()
			prepared, e := f.store.PrepareMigration(ctx, req)
			require.NoError(t, e)
			if phase == "sealed" || phase == "committed" {
				seal, e := f.store.SealPrepared(ctx, req)
				require.NoError(t, e)
				if phase == "committed" {
					_, e = f.store.CommitPreparedPins(ctx, req, seal.Revision)
					require.NoError(t, e)
				}
			}
			if phase == "cancelled" {
				_, e = f.store.CancelMigrationShadow(ctx, source.OperationID, receipt.RequestHash, receipt.CheckpointHash)
				require.NoError(t, e)
			}
			r := lifecycleHTTPRouter(t, f)
			base := "/api/channel-migrations/operations/" + source.OperationID + "/shadow"
			_, got, raw := lifecycleHTTPCall(t, r, "GET", base+"/lifecycle", "", config.AdminKey)
			require.Equal(t, "ready", got.State)
			require.Equal(t, source, got.Observation.ResourceRequest)
			require.Equal(t, *receipt, got.Observation.Creation)
			require.Equal(t, prepared.RequestHash, got.Observation.PreparedBinding.RequestHash)
			require.Equal(t, receipt.CheckpointHash, got.Observation.PreparedBinding.ShadowCheckpointHash)
			require.NotContains(t, raw, `"currentHead"`)
			if phase == "sealed" || phase == "committed" {
				_, cancelled, _ := lifecycleHTTPCall(t, r, "POST", base+"/cancel", lifecycleCancelJSON(t, receipt), config.AdminKey)
				require.Equal(t, "conflict", cancelled.State)
				require.Nil(t, cancelled.Observation)
			} else {
				_, cancelled, _ := lifecycleHTTPCall(t, r, "POST", base+"/cancel", lifecycleCancelJSON(t, receipt), config.AdminKey)
				require.Equal(t, "ready", cancelled.State)
				require.Equal(t, "abandoned", cancelled.Observation.PreparedBinding.State)
			}
		})
	}
}
func TestMigrationShadowLifecycleHTTPNeverConfusesMissingHistory(t *testing.T) {
	for _, mode := range []string{"resource-lost", "prepared-lost", "wrong-binding", "unknown", "partial", "version", "target-deleted", "target-enabled", "source-drift", "decimal"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdmissionFixture(t)
			source, receipt, req := lifecycleFixture(t, f)
			ctx := context.Background()
			db := f.store.DB
			if mode == "resource-lost" || mode == "prepared-lost" || mode == "wrong-binding" {
				_, e := f.store.PrepareMigration(ctx, req)
				require.NoError(t, e)
			}
			switch mode {
			case "resource-lost":
				require.NoError(t, db.Exec("DELETE FROM channel_migration_shadow_resources").Error)
			case "prepared-lost":
				require.NoError(t, db.Exec("DELETE FROM channel_migration_prepareds").Error)
			case "wrong-binding":
				require.NoError(t, db.Exec("UPDATE channel_migration_prepareds SET shadow_checkpoint_hash=''").Error)
			case "unknown":
				require.NoError(t, db.Exec("UPDATE channel_migration_shadow_resources SET lifecycle_state='unknown'").Error)
			case "partial":
				require.NoError(t, db.Migrator().DropTable(&model.ChannelMigrationShadowProtocol{}))
			case "version":
				require.NoError(t, db.Exec("UPDATE channel_migration_shadow_protocols SET version=1").Error)
			case "target-deleted":
				require.NoError(t, db.Exec("DELETE FROM channels WHERE id=?", receipt.Shadow.ChannelID).Error)
			case "target-enabled":
				require.NoError(t, db.Exec("UPDATE channels SET status=1 WHERE id=?", receipt.Shadow.ChannelID).Error)
			case "source-drift":
				require.NoError(t, db.Exec("UPDATE model_configs SET rpm=999").Error)
			case "decimal":
				require.NoError(t, db.Exec("UPDATE channel_admission_controls SET revision=?", int64(9007199254740993)).Error)
			}
			r := lifecycleHTTPRouter(t, f)
			_, got, _ := lifecycleHTTPCall(t, r, "GET", "/api/channel-migrations/operations/"+source.OperationID+"/shadow/lifecycle", "", config.AdminKey)
			if mode == "source-drift" || mode == "decimal" {
				require.Equal(t, "ready", got.State)
				if mode == "decimal" {
					require.Equal(t, "9007199254740993", got.CurrentRevision)
				}
			} else {
				require.NotEqual(t, "ready", got.State)
				require.NotEqual(t, "absent", got.State)
				require.Nil(t, got.Observation)
			}
		})
	}
}
func TestMigrationShadowLifecycleHTTPTransportLostCancelResponse(t *testing.T) {
	f := newAdmissionFixture(t)
	source, receipt, req := lifecycleFixture(t, f)
	_, e := f.store.PrepareMigration(context.Background(), req)
	require.NoError(t, e)
	server := httptest.NewServer(lifecycleHTTPRouter(t, f))
	defer server.Close()
	base := server.URL + "/api/channel-migrations/operations/" + source.OperationID + "/shadow"
	call := func(method, path, body string) *http.Response {
		request, e := http.NewRequest(method, path, strings.NewReader(body))
		require.NoError(t, e)
		request.Header.Set("Authorization", "Bearer "+config.AdminKey)
		response, e := server.Client().Do(request)
		require.NoError(t, e)
		require.Equal(t, 200, response.StatusCode)
		return response
	}
	response := call("POST", base+"/cancel", lifecycleCancelJSON(t, receipt))
	io.Copy(io.Discard, response.Body)
	response.Body.Close() // caller loses returned receipt
	response = call("GET", base+"/lifecycle", "")
	var read struct {
		Data model.MigrationShadowLifecycleResult `json:"data"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&read))
	response.Body.Close()
	require.Equal(t, "cancelled", read.Data.Observation.Lifecycle.State)
	require.Equal(t, "abandoned", read.Data.Observation.PreparedBinding.State)
	response = call("POST", base+"/cancel", lifecycleCancelJSON(t, receipt))
	var replay struct {
		Data model.MigrationShadowLifecycleResult `json:"data"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&replay))
	response.Body.Close()
	require.Equal(t, read, replay)
	_, e = strconv.ParseInt(replay.Data.CurrentRevision, 10, 64)
	require.NoError(t, e)
}

func TestMigrationShadowLifecycleHTTPReadSQLiteReadOnly(t *testing.T) {
	f := newAdmissionFixture(t)
	source, _, req := lifecycleFixture(t, f)
	_, e := f.store.PrepareMigration(context.Background(), req)
	require.NoError(t, e)
	pool, e := f.store.DB.DB()
	require.NoError(t, e)
	pool.SetMaxOpenConns(1)
	require.NoError(t, f.store.DB.Exec("PRAGMA query_only=ON").Error)
	r := lifecycleHTTPRouter(t, f)
	_, got, _ := lifecycleHTTPCall(t, r, "GET", "/api/channel-migrations/operations/"+source.OperationID+"/shadow/lifecycle", "", config.AdminKey)
	require.Equal(t, "ready", got.State)
	_, missing, _ := lifecycleHTTPCall(t, r, "GET", "/api/channel-migrations/operations/absent/shadow/lifecycle", "", config.AdminKey)
	require.Equal(t, "absent", missing.State)
}

func TestMigrationShadowLifecycleHTTPStorageUnavailableNeverAbsent(t *testing.T) {
	for _, mode := range []string{"extension-absent", "missing-column", "db-split"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdmissionFixture(t)
			source, _, _ := lifecycleFixture(t, f)
			switch mode {
			case "extension-absent":
				require.NoError(t, f.store.DB.Migrator().DropTable(&model.ChannelMigrationShadowResource{}, &model.ChannelMigrationShadowProtocol{}))
			case "missing-column":
				require.NoError(t, f.store.DB.Migrator().DropColumn(&model.ChannelMigrationPrepared{}, "shadow_checkpoint_hash"))
			case "db-split":
				f.store.LogDB = newAdmissionFixture(t).store.DB
			}
			got := f.store.ReadMigrationShadowLifecycleFacts(context.Background(), source.OperationID)
			require.NotEqual(t, "absent", got.State)
			require.NotEqual(t, "ready", got.State)
			require.Nil(t, got.Observation)
		})
	}
}

func TestMigrationShadowLifecycleHTTPOriginalRequestBinding(t *testing.T) {
	for _, mode := range []string{"source-drift", "known-secret", "request-corrupt", "unsafe-config"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdmissionFixture(t)
			source := shadowFixture(t, f)
			original := `{"outbound_policy":"public_only"}`
			source.Target.Projection.ConfigsJSON = &original
			got := f.store.CreateMigrationShadow(context.Background(), source, shadowTestKey, nil)
			require.Equal(t, "ready", got.State)
			switch mode {
			case "source-drift":
				require.NoError(t, f.store.DB.Exec("UPDATE model_configs SET rpm=999").Error)
			case "known-secret":
				require.NoError(t, f.store.DB.Exec("UPDATE channels SET key=? WHERE id=5", source.Target.Projection.Name).Error)
			case "request-corrupt":
				require.NoError(t, f.store.DB.Exec("UPDATE channel_migration_shadow_resources SET request_json=?", `{"version":1}`).Error)
			case "unsafe-config":
				raw, e := json.Marshal(source)
				require.NoError(t, e)
				modified := strings.Replace(string(raw), `outbound_policy`, `authentication_token`, 1)
				require.NoError(t, f.store.DB.Exec("UPDATE channel_migration_shadow_resources SET request_json=?", modified).Error)
			}
			r := lifecycleHTTPRouter(t, f)
			_, read, raw := lifecycleHTTPCall(t, r, "GET", "/api/channel-migrations/operations/"+source.OperationID+"/shadow/lifecycle", "", config.AdminKey)
			if mode == "source-drift" {
				require.Equal(t, "ready", read.State)
				require.Equal(t, source, read.Observation.ResourceRequest)
				require.Equal(t, original, *read.Observation.ResourceRequest.Target.Projection.ConfigsJSON)
				// A first create response was lost; complete original request proves the
				// retained resource belongs to this intent despite unrelated source drift.
				cancelled := f.store.CancelMigrationShadowFacts(context.Background(), source.OperationID, read.Observation.Creation.RequestHash, read.Observation.Creation.CheckpointHash)
				require.Equal(t, "ready", cancelled.State)
				require.Equal(t, source, cancelled.Observation.ResourceRequest)
			} else {
				require.NotEqual(t, "ready", read.State)
				require.NotEqual(t, "absent", read.State)
				require.Nil(t, read.Observation)
				require.NotContains(t, raw, source.Target.Projection.Name)
			}
		})
	}
}
