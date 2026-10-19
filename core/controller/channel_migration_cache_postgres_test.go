//nolint:testpackage // White-box coverage needs the package-private mustJSON fixture or implementation.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Test-only subprocess harness. Never installed by runtime startup or a config
// switch. The synthetic proof exercises admission, not real fleet membership.
func TestMigrationCacheChildProcess(t *testing.T) {
	dir := os.Getenv("B2_CACHE_CHILD_DIR")
	if dir == "" {
		t.Skip("parent-owned loopback process")
	}

	dsn := os.Getenv("B2_CACHE_CHILD_DSN")
	u, e := url.Parse(dsn)
	require.NoError(t, e)
	require.Equal(t, "127.0.0.1", u.Hostname())
	require.Equal(t, "/g3_queue_test", u.Path)

	db, e := model.OpenPostgreSQL(dsn)
	require.NoError(t, e)
	pool, e := db.DB()
	require.NoError(t, e)

	defer pool.Close()

	model.DB, model.LogDB = db, db
	config.AdminKey = "synthetic-cache-admin"

	var proof model.VerifiedDeploymentProof
	require.NoError(t, json.Unmarshal([]byte(os.Getenv("B2_CACHE_CHILD_PROOF")), &proof))

	var from model.ChannelIdentity
	require.NoError(t, json.Unmarshal([]byte(os.Getenv("B2_CACHE_CHILD_FROM")), &from))

	store := &model.ChannelAdmissionStore{
		DB:             db,
		LogDB:          db,
		Executor:       proof.Executors[0],
		LoadDeployment: func(context.Context) (model.VerifiedDeploymentProof, error) { return proof, nil },
	}

	require.NoError(t, model.InitModelConfigAndChannelCache())
	gin.SetMode(gin.ReleaseMode)

	router := gin.New()
	api := router.Group(
		"/api/channel-migrations",
		RequireMigrationInventoryHeaderAuth,
		middleware.AdminAuth,
	)
	api.GET("/operations/:id/cache", GetMigrationCacheObservation)
	// Local test-only commands invoke the real rebuild and admission functions.
	router.POST("/harness/rebuild", func(c *gin.Context) {
		if e := model.InitModelConfigAndChannelCache(); e != nil {
			c.Status(500)
			return
		}

		c.Status(204)
	})
	router.POST("/harness/admit-old", func(c *gin.Context) {
		ch := model.LoadModelCaches().ChannelsByID[from.ChannelID]

		id, e := model.ChannelIdentityOf(ch)
		if e != nil {
			c.Status(500)
			return
		}

		_, _, e = store.Admit(
			c.Request.Context(),
			model.ChannelAdmissionRequest{
				RequestID:   "synthetic-old-cache-attempt",
				GroupID:     "internal-probe",
				TokenID:     1,
				Lane:        "native",
				RequestHash: strings.Repeat("a", 64),
				Identity:    id,
			},
			nil,
		)
		if e == nil {
			c.Status(201)
			return
		}

		c.Status(409)
	})

	server := httptest.NewServer(router)
	defer server.Close()

	boot, e := model.ProcessBootIncarnation()
	require.NoError(t, e)

	metadata := mustJSON(t, map[string]string{"url": server.URL, "boot": boot})

	// #nosec G703 -- The opt-in local integration harness owns this fixture or rendezvous path; no HTTP input supplies it.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ready"), metadata, 0o600))
	defer func() {
		// #nosec G703 -- The opt-in local integration harness owns this fixture or rendezvous path; no HTTP input supplies it.
		if err := os.Remove(filepath.Join(dir, "ready")); err != nil {
			t.Error(err)
		}
	}()

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		// #nosec G703 -- The opt-in local integration harness owns this fixture or rendezvous path; no HTTP input supplies it.
		if _, e := os.Stat(filepath.Join(dir, "stop")); e == nil {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("parent did not stop local fixture")
}

type cacheChild struct {
	url, boot string
	call      func(string, string) (int, []byte)
	stop      func()
}

func startCacheChild(t *testing.T, f *probeHTTPFixture) cacheChild {
	t.Helper()

	var schema string
	require.NoError(t, f.db.Raw("SELECT current_schema()").Scan(&schema).Error)

	u, e := url.Parse(os.Getenv("B2_G3_TEST_POSTGRES_DSN"))
	require.NoError(t, e)

	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	proof, e := f.store.LoadDeployment(t.Context())
	require.NoError(t, e)

	pr := mustJSON(t, proof)
	from := mustJSON(t, f.body.Command.Seal.From)
	dir := t.TempDir()
	binary, e := os.Executable()
	require.NoError(t, e)

	cmd := exec.CommandContext(t.Context(),
		binary,
		"-test.run=^TestMigrationCacheChildProcess$",
		"-test.count=1",
		"-test.timeout=100s",
	)

	cmd.Env = append(
		os.Environ(),
		"B2_CACHE_CHILD_DIR="+dir,
		"B2_CACHE_CHILD_DSN="+u.String(),
		"B2_CACHE_CHILD_PROOF="+string(pr),
		"B2_CACHE_CHILD_FROM="+string(from),
	)

	var logs bytes.Buffer

	cmd.Stdout = &logs
	cmd.Stderr = &logs
	require.NoError(t, cmd.Start())

	var stopOnce sync.Once

	stop := func() {
		stopOnce.Do(func() {
			_ = os.WriteFile(filepath.Join(dir, "stop"), nil, 0o600)

			require.NoError(t, cmd.Wait(), logs.String())
			require.NoFileExists(t, filepath.Join(dir, "ready"))
		})
	}
	t.Cleanup(stop)

	var metadata map[string]string
	require.Eventually(t, func() bool {
		b, e := os.ReadFile(filepath.Join(dir, "ready"))
		return e == nil && json.Unmarshal(b, &metadata) == nil
	}, 10*time.Second, 25*time.Millisecond)

	call := func(method, path string) (int, []byte) {
		r, e := http.NewRequestWithContext(t.Context(), method, metadata["url"]+path, nil)
		require.NoError(t, e)
		r.Header.Set("Authorization", "Bearer synthetic-cache-admin")

		client := &http.Client{Timeout: 5 * time.Second}
		resp, e := client.Do(r)
		require.NoError(t, e)

		defer resp.Body.Close()

		b, e := io.ReadAll(resp.Body)
		require.NoError(t, e)

		return resp.StatusCode, b
	}

	return cacheChild{metadata["url"], metadata["boot"], call, stop}
}

func childCache(t *testing.T, c cacheChild) model.MigrationCacheObservationResult {
	t.Helper()

	code, b := c.call("GET", cachePath)
	require.Equal(t, 200, code)

	var v struct {
		Data model.MigrationCacheObservationResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(b, &v))

	return v.Data
}

func TestPostgresMigrationCacheTwoProcesses(t *testing.T) {
	f := newProbeHTTPFixtureDB(t, cutoverHTTPPostgres(t))
	b := prepareHTTPProbeForCutover(t, f)
	a := startCacheChild(t, f)
	bProcess := startCacheChild(t, f)
	require.NotEqual(t, a.boot, bProcess.boot)
	require.Equal(t, "pending", childCache(t, a).State)
	require.Equal(t, "pending", childCache(t, bProcess).State)
	_, raw := f.call(
		t,
		"POST",
		"/api/channel-migrations/operations/http-probe-op/cutover",
		b,
		config.AdminKey,
	)
	require.Contains(t, string(raw), `"state":"committed"`)
	require.Equal(t, "pending", childCache(t, a).State)
	require.Equal(t, "pending", childCache(t, bProcess).State)
	status, _ := bProcess.call("POST", "/harness/admit-old")
	require.Equal(t, 409, status, "old actual cache cannot bypass the persistent barrier")
	status, _ = a.call("POST", "/harness/rebuild")
	require.Equal(t, 204, status)
	loaded := childCache(t, a)
	require.Equal(t, "loaded", loaded.State, loaded.Reason)
	require.Equal(
		t,
		"pending",
		childCache(t, bProcess).State,
		"two HTTP responses from A do not acknowledge B",
	)
	status, _ = bProcess.call("POST", "/harness/rebuild")
	require.Equal(t, 204, status)
	require.Equal(t, "loaded", childCache(t, bProcess).State)
	a.stop()

	restarted := startCacheChild(t, f)
	require.NotEqual(t, a.boot, restarted.boot)
	require.NotEqual(t, bProcess.boot, restarted.boot)
	next := childCache(t, restarted)
	require.Equal(t, "loaded", next.State, next.Reason)
	require.Equal(t, "1", next.Observation.CacheVersion)
	require.Equal(t, loaded.Observation.AfterPinHash, next.Observation.AfterPinHash)
	require.NoError(
		t,
		f.db.Model(&model.ChannelAdmissionFamily{}).
			Where("operation_id=?", "http-probe-op").
			UpdateColumn("operation_id", "superseded").
			Error,
	)
	require.Equal(t, "conflict", childCache(t, bProcess).State)
}

func TestPostgresMigrationCacheNoDML(t *testing.T) {
	f := newProbeHTTPFixtureDB(t, cutoverHTTPPostgres(t))
	retentionCommitted(t, f)
	require.NoError(
		t,
		f.db.Exec(
			`CREATE FUNCTION cache_no_dml() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'cache must not write'; END $$`,
		).Error,
	)

	var tables []string
	require.NoError(
		t,
		f.db.Raw("SELECT tablename FROM pg_tables WHERE schemaname=current_schema()").
			Scan(&tables).
			Error,
	)

	for i, table := range tables {
		require.NoError(
			t,
			f.db.Exec(
				fmt.Sprintf(
					"CREATE TRIGGER cache_no_dml_%d BEFORE INSERT OR UPDATE OR DELETE ON %s FOR EACH STATEMENT EXECUTE FUNCTION cache_no_dml()",
					i,
					table,
				),
			).Error,
		)
	}

	require.NoError(t, model.InitModelConfigAndChannelCache())
	require.Equal(t, "loaded", cacheRead(t, f).State)
	require.Error(t, f.db.Exec("UPDATE channel_admission_controls SET revision=revision").Error)
}

func TestPostgresMigrationCacheSnapshot(t *testing.T) {
	cacheConsistentSnapshot(t, newProbeHTTPFixtureDB(t, cutoverHTTPPostgres(t)))
}

func TestMigrationCacheSQLiteSnapshot(t *testing.T) {
	f := newProbeHTTPFixture(t)
	require.NoError(t, f.db.Exec("PRAGMA journal_mode=WAL").Error)
	cacheConsistentSnapshot(t, f)
}

func cacheConsistentSnapshot(t *testing.T, f *probeHTTPFixture) {
	t.Helper()
	b := prepareHTTPProbeForCutover(t, f)
	require.NoError(t, model.InitModelConfigAndChannelCache())

	reached, release := make(chan struct{}), make(chan struct{})
	second, releaseSecond := make(chan struct{}), make(chan struct{})

	var count atomic.Int64

	require.NoError(
		t,
		f.db.Callback().Query().After("gorm:query").Register("cache:consistent", func(tx *gorm.DB) {
			if tx.Statement.Table == "model_configs" &&
				strings.Contains(tx.Statement.SQL.String(), "created_at desc") {
				switch count.Add(1) {
				case 1:
					close(reached)
					<-release
				case 2:
					close(second)
					<-releaseSecond
				}
			}
		}),
	)
	defer func() {
		if err := f.db.Callback().Query().Remove("cache:consistent"); err != nil {
			t.Error(err)
		}
	}()

	rebuilt := make(chan error, 1)
	go func() { rebuilt <- model.InitModelConfigAndChannelCache() }()

	select {
	case <-reached:
	case <-time.After(3 * time.Second):
		t.Fatal("missing snapshot boundary")
	}

	cutover := make(chan []byte, 1)
	go func() {
		_, raw := f.call(
			t,
			"POST",
			"/api/channel-migrations/operations/http-probe-op/cutover",
			b,
			config.AdminKey,
		)
		cutover <- raw
	}()

	require.Eventually(t, func() bool {
		var n int64

		_ = f.db.Model(&model.ChannelMigrationPrepared{}).
			Where("state=?", "committed").
			Count(&n).
			Error

		return n == 1
	}, 5*time.Second, 10*time.Millisecond)
	// The first rebuild read old control/configs before a concurrent committed
	// cutover. It must not attach the new head to mixed old/new routing.
	close(release)
	require.NoError(t, <-rebuilt)

	select {
	case <-second:
	case <-time.After(3 * time.Second):
		t.Fatal("second rebuild did not start")
	}

	require.Equal(
		t,
		model.ChannelStatusDisabled,
		model.LoadModelCaches().ChannelsByID[f.body.Command.Seal.To.ChannelID].Status,
	)
	require.Equal(t, "pending", cacheRead(t, f).State)
	close(releaseSecond)
	require.Contains(t, string(<-cutover), `"state":"committed"`)
	require.Equal(t, "loaded", cacheRead(t, f).State)
}
