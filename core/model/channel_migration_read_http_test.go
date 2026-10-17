package model_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/router"
	"github.com/stretchr/testify/require"
)

func migrationReadHTTPFixture(t *testing.T) (*gin.Engine, *admissionFixture, model.ChannelMigrationRequest) {
	t.Helper()
	f := newAdmissionFixture(t)
	req, _ := migrationFixture(t, f, 3)
	ctx := context.Background()
	_, err := f.store.PrepareMigration(ctx, req)
	require.NoError(t, err)
	seal, err := f.store.SealPrepared(ctx, req)
	require.NoError(t, err)
	_, err = f.store.CommitPreparedPins(ctx, req, seal.Revision)
	require.NoError(t, err)
	oldDB, oldLog, oldKey := model.DB, model.LogDB, config.AdminKey
	model.DB, model.LogDB, config.AdminKey = f.store.DB, f.store.DB, "synthetic-read-http-admin"
	t.Cleanup(func() { model.DB, model.LogDB, config.AdminKey = oldDB, oldLog, oldKey })
	r := gin.New()
	router.SetAPIRouter(r)
	return r, f, req
}
func TestMigrationReadHTTPAuthAndStrictInput(t *testing.T) {
	r, _, req := migrationReadHTTPFixture(t)
	call := func(method, path, body, key string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		out := httptest.NewRecorder()
		r.ServeHTTP(out, request)
		require.Equal(t, "no-store", out.Header().Get("Cache-Control"))
		require.NotContains(t, out.Body.String(), "synthetic-old")
		return out
	}
	for _, path := range []string{"/api/channel-migrations/protocol", "/api/channel-migrations/operations/prepared"} {
		require.Equal(t, 401, call("GET", path, "", "").Code)
		require.Equal(t, 401, call("GET", path, "", "wrong").Code)
		require.Equal(t, 401, call("GET", path+"?key=synthetic-read-http-admin", "", config.AdminKey).Code)
		require.Equal(t, 400, call("GET", path, "{}", config.AdminKey).Code)
		out := call("GET", path, "", config.AdminKey)
		require.Equal(t, 200, out.Code)
		require.Contains(t, out.Body.String(), `"state":"ready"`)
	}
	raw, err := json.Marshal(map[string]any{"from": req.Seal.From, "to": req.Seal.To})
	require.NoError(t, err)
	path := "/api/channel-migrations/snapshot/read"
	require.Equal(t, 401, call("POST", path, string(raw), "").Code)
	require.Equal(t, 200, call("POST", path, string(raw), config.AdminKey).Code)
	for _, body := range []string{`{}`, string(raw) + `{}`, `{"from":{},"to":{},"unknown":true}`, `{"from":{},"from":{},"to":{}}`, strings.Replace(string(raw), `"from"`, `"From"`, 1), strings.Repeat(" ", 4097), strings.Replace(string(raw), `"channelId":1`, `"channelId":9223372036854775808`, 1)} {
		require.Equal(t, 400, call("POST", path, body, config.AdminKey).Code, body)
	}
	require.Equal(t, 400, call("GET", "/api/channel-migrations/operations/"+strings.Repeat("a", 129), "", config.AdminKey).Code)
	for _, id := range []string{".dot", ":colon", "-dash", "_underscore"} {
		require.Equal(t, 400, call("GET", "/api/channel-migrations/operations/"+id, "", config.AdminKey).Code)
	}
	missing := call("GET", "/api/channel-migrations/operations/absent-op", "", config.AdminKey)
	require.Contains(t, missing.Body.String(), `"state":"absent"`)
	// No writes are exposed by this router group.
	write := httptest.NewRecorder()
	r.ServeHTTP(write, httptest.NewRequest("POST", "/api/channel-migrations/operations/prepared/seal", bytes.NewReader([]byte(`{}`))))
	require.Equal(t, 404, write.Code)
}

// Opt-in loopback server for the app agent's actual typed HTTP reader. Fixture
// DB, admin key and deployment/probe records are synthetic and local only.
func TestMigrationReadHTTPInteropServer(t *testing.T) {
	dir := os.Getenv("B2_MIGRATION_READ_HTTP_FIXTURE_DIR")
	if dir == "" {
		t.Skip("explicit isolated interop directory required")
	}
	require.True(t, strings.HasPrefix(dir, "/private/tmp/"))
	r, _, req := migrationReadHTTPFixture(t)
	server := httptest.NewServer(r)
	defer server.Close()
	require.NoError(t, os.MkdirAll(dir, 0700))
	raw, err := json.Marshal(map[string]any{"baseUrl": server.URL, "adminKey": config.AdminKey, "operationId": req.Seal.OperationID, "absentOperationId": "absent-op", "from": req.Seal.From, "to": req.Seal.To})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "connection.json"), raw, 0600))
	defer os.Remove(filepath.Join(dir, "connection.json"))
	until := time.Now().Add(10 * time.Minute)
	for time.Now().Before(until) {
		if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("interop server was not stopped")
}
