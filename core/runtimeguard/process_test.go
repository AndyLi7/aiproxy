//nolint:testpackage // White-box regression exercises private model or runtime invariants.
package runtimeguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Actual child processes run the same bootstrap/Configure/Activate and model
// admission. The only HTTP endpoints here are local test probes, not providers.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "--controlled-runtime-read-fd=") {
		os.Exit(childProcess())
	}

	os.Exit(m.Run())
}

func childProcess() int {
	in, out := os.NewFile(3, "parent-in"), os.NewFile(4, "parent-out")
	defer in.Close()
	defer out.Close()

	h, r, boot, e := ChildHandshake(in, out)
	if e != nil {
		return 2
	}

	db, e := OpenControlledStorage(h)
	if e != nil {
		return 3
	}

	model.DB = db
	model.LogDB = db

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan error, 1)

	done := make(chan error, 1)
	go func() { e := RunChild(ctx, h, r, in, out, boot, ready); done <- e; cancel() }()

	if e = <-ready; e != nil {
		return 4
	}

	var address string
	for _, arg := range os.Args[1:] {
		if after, ok := strings.CutPrefix(arg, "--listen="); ok {
			address = after
		}
	}

	ln, e := (&net.ListenConfig{}).
		Listen(ctx, "tcp", address)
	if e != nil {
		return 5
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/admit", func(w http.ResponseWriter, q *http.Request) {
		s, e := model.MigrationProbeRuntimeStore(db, db, nil)
		if e != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}

		var c model.Channel
		db.First(&c, 1)
		id, _ := model.ChannelIdentityOf(&c)

		_, _, e = s.Admit(
			q.Context(),
			model.ChannelAdmissionRequest{
				RequestID:   q.URL.Query().Get("id"),
				RequestHash: strings.Repeat("a", 64),
				Lane:        "native",
				GroupID:     "local",
				TokenID:     1,
				Identity:    id,
			},
			nil,
		)
		if e != nil {
			http.Error(w, "refused", http.StatusConflict)
			return
		}

		fmt.Fprint(w, "accepted")
	})
	mux.HandleFunc("/migration", func(w http.ResponseWriter, q *http.Request) {
		s, e := model.MigrationProbeRuntimeStore(db, db, nil)
		if e != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}

		_, a := s.Seal(q.Context(), model.ChannelSealRequest{})
		_, b := s.PrepareMigration(q.Context(), model.ChannelMigrationRequest{})
		_, c := s.CommitPreparedPins(q.Context(), model.ChannelMigrationRequest{}, 1)

		_, d := s.CutoverPrepared(q.Context(), model.ChannelMigrationRequest{})
		if a == nil || b == nil || c == nil || d == nil {
			http.Error(w, "unexpected permission", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusForbidden)
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() {
		if serveErr := srv.Serve(
			ln,
		); serveErr != nil &&
			!errors.Is(serveErr, http.ErrServerClosed) {
			cancel()
		}
	}()

	<-ctx.Done()

	_ = srv.Close()

	if childErr := <-done; childErr != nil && !errors.Is(childErr, context.Canceled) {
		return 6
	}

	return 0
}

func seed(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seed.sqlite")
	db, e := model.OpenSQLite(path)
	require.NoError(t, e)
	require.NoError(
		t,
		db.AutoMigrate(
			&model.Channel{},
			&model.ModelConfig{},
			&model.Group{},
			&model.Token{},
			&model.Option{},
			&model.NativeTask{},
			&model.ImageTask{},
			&model.AsyncUsageInfo{},
			&model.Log{},
			&model.ChannelAdmissionExecution{},
			&model.ChannelAdmissionExecutionProtocol{},
			&model.ChannelAdmissionTaskExecution{},
			&model.ChannelAdmissionDispatchHandoff{},
			&model.ChannelAdmissionRecoveryHandoff{},
			&model.ChannelAdmissionImageRecoveryClaim{},
			&model.ChannelAdmissionImageRecoveryHandoff{},
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
	require.NoError(t, db.Create(&model.ChannelAdmissionControl{ID: 1, Version: 1}).Error)
	require.NoError(t, db.Create(&model.ChannelMigrationProtocol{ID: 1, Version: 1}).Error)
	require.NoError(
		t,
		db.Create(
			&model.Channel{
				ID:      1,
				Name:    "synthetic-local",
				Key:     "synthetic-key",
				Type:    model.ChannelTypeFal,
				Status:  model.ChannelStatusEnabled,
				BaseURL: "https://supplier.invalid",
			},
		).Error,
	)
	require.NoError(t, model.CheckControlledFreshStorage(db))
	pool, _ := db.DB()
	require.NoError(t, pool.Close())
	require.NoError(t, os.Chmod(path, 0o600))

	return path
}

func port(t *testing.T) string {
	t.Helper()

	l, e := (&net.ListenConfig{}).
		Listen(t.Context(),

			"tcp", "127.0.0.1:0")
	require.NoError(t, e)

	a := l.Addr().String()
	l.Close()

	return a
}

func launch(t *testing.T) (Launch, string) {
	t.Helper()
	seedPath := seed(t)
	exe, e := os.Executable()
	require.NoError(t, e)
	b, e := os.ReadFile(exe)
	require.NoError(t, e)
	raw, e := os.ReadFile(seedPath)
	require.NoError(t, e)

	until := time.Now().Add(time.Minute).UnixMilli()

	return Launch{
		Directory:              filepath.Join(t.TempDir(), "session"),
		Seed:                   seedPath,
		SeedSHA256:             digest(raw),
		Executable:             exe,
		ExecutableSHA256:       digest(b),
		Members:                []Member{{"gateway-a", port(t)}, {"gateway-b", port(t)}},
		ValidUntilMs:           until,
		RevocationFreshUntilMs: until,
	}, seedPath
}

func requestStatus(t *testing.T, url string, want int) {
	t.Helper()

	client := http.Client{Timeout: time.Second}

	var got int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		require.NoError(t, requestErr)

		r, e := client.Do(request)
		if e == nil {
			got = r.StatusCode
			r.Body.Close()

			if got == want {
				return
			}
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("status %d expected %d", got, want)
}

func TestActualSupervisedProcessesAdmissionRenewRevokeAndRestoreRefusal(t *testing.T) {
	c, _ := launch(t)
	s, e := Start(context.Background(), c)
	require.NoError(t, e)
	t.Cleanup(func() { _ = s.Stop() })
	require.Len(t, s.children, 2)
	require.NotEqual(t, s.children[0].boot.BootIncarnation, s.children[1].boot.BootIncarnation)

	for _, m := range c.Members {
		requestStatus(t, "http://"+m.Listen+"/admit?id="+m.ServiceID, 200)
		requestStatus(t, "http://"+m.Listen+"/migration", 403)
	}

	old := s.proof
	until := time.Now().Add(time.Minute).UnixMilli()
	require.NoError(t, s.Renew(until, until))
	require.Equal(t, old.Sequence+1, s.proof.Sequence)
	require.NotEqual(t, old.ProofID, s.proof.ProofID)
	require.Equal(t, old.MembershipHash, s.proof.MembershipHash)

	for _, m := range c.Members {
		requestStatus(t, "http://"+m.Listen+"/admit?id=renew-"+m.ServiceID, 200)
	}

	require.NoError(t, s.Stop())

	db, e := model.OpenSQLite(filepath.Join(c.Directory, "runtime.sqlite"))
	require.NoError(t, e)

	defer func() { p, _ := db.DB(); p.Close() }()

	var proof model.ChannelAdmissionProof
	require.NoError(t, db.First(&proof, "id=?", s.proof.ProofID).Error)
	require.True(t, proof.Revoked)
	require.Error(t, model.CheckControlledFreshStorage(db))

	var uses int64
	require.NoError(t, db.Model(&model.ChannelAdmissionUse{}).Count(&uses).Error)
	require.Equal(t, int64(4), uses)
	// The original zero-revision seed may be a database prefix, but cannot reopen
	// this independent namespace or replay its old issued authority.
	_, e = Start(context.Background(), c)
	require.Error(t, e)
}

func TestControlledSeedRejectsHistoryPartialAndArtifactChange(t *testing.T) {
	for _, mode := range []string{"history", "partial", "artifact", "mutable-seed"} {
		t.Run(mode, func(t *testing.T) {
			c, p := launch(t)
			switch mode {
			case "artifact":
				c.ExecutableSHA256 = strings.Repeat("0", 64)
			case "mutable-seed":
				require.NoError(t, os.Chmod(p, 0o666))
			default:
				db, e := model.OpenSQLite(p)
				require.NoError(t, e)

				if mode == "history" {
					require.NoError(
						t,
						db.Create(
							&model.ChannelAdmissionProof{
								ID:               "prior",
								FactHash:         strings.Repeat("a", 64),
								ConsumedSequence: 1,
							},
						).Error,
					)
				} else {
					require.NoError(t, db.Migrator().DropTable(&model.ChannelMigrationPrepared{}))
				}

				pool, _ := db.DB()
				pool.Close()

				b, _ := os.ReadFile(p)
				c.SeedSHA256 = digest(b)
			}

			s, e := Start(context.Background(), c)
			require.Error(t, e)
			require.Nil(t, s)
		})
	}
}

var _ *gorm.DB

func TestActualProofExpiryDoesNotRefreshOrCloseOldUse(t *testing.T) {
	c, _ := launch(t)
	s, e := Start(context.Background(), c)
	require.NoError(t, e)
	t.Cleanup(func() { _ = s.Stop() })
	requestStatus(t, "http://"+c.Members[0].Listen+"/admit?id=before", 200)

	until := time.Now().Add(400 * time.Millisecond).UnixMilli()
	require.NoError(t, s.Renew(until, until))
	time.Sleep(time.Until(time.UnixMilli(until).Add(30 * time.Millisecond)))
	requestStatus(t, "http://"+c.Members[0].Listen+"/admit?id=after", 409)
	require.NoError(t, s.Stop())

	db, e := model.OpenSQLite(filepath.Join(c.Directory, "runtime.sqlite"))
	require.NoError(t, e)

	defer func() { p, _ := db.DB(); p.Close() }()

	var n int64
	require.NoError(t, db.Model(&model.ChannelAdmissionUse{}).Where("state='open'").Count(&n).Error)
	require.Equal(t, int64(1), n)
}

func TestActualLostChildStopsFixedSetAndDoesNotClaimCleanExit(t *testing.T) {
	c, _ := launch(t)
	s, e := Start(context.Background(), c)
	require.NoError(t, e)
	require.NoError(t, s.children[0].cmd.Process.Kill())

	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("lost child did not stop set")
	}

	require.Error(t, s.Stop())

	for _, ch := range s.children {
		select {
		case <-ch.done:
		default:
			t.Fatal("child not joined")
		}
	}

	raw, e := os.ReadFile(filepath.Join(c.Directory, "issuance.log"))
	require.NoError(t, e)
	require.NotContains(t, string(raw), "runtime_stopped")
	require.Contains(t, string(raw), "proof_revoked")
}

func TestActualGatewayMainControlledBootstrap(t *testing.T) {
	binary := os.Getenv("B2_CONTROLLED_GATEWAY_BINARY")
	if binary == "" {
		t.Skip("built gateway binary required")
	}

	c, _ := launch(t)
	c.Executable = binary
	//nolint:gosec // Local integration harness path comes from the test operator, never an HTTP request.
	b, e := os.ReadFile(binary)
	require.NoError(t, e)

	c.ExecutableSHA256 = digest(b)

	s, e := Start(context.Background(), c)
	if e != nil {
		for i := range c.Members {
			raw, _ := os.ReadFile(filepath.Join(c.Directory, fmt.Sprintf("child-%d.log", i)))
			t.Logf("child %d synthetic startup log: %s", i, raw)
		}
	}

	require.NoError(t, e)
	t.Cleanup(func() { _ = s.Stop() })

	for _, m := range c.Members {
		requestStatus(t, "http://"+m.Listen+"/api/status", 200)
	}

	require.NoError(t, s.Stop())
}

func TestActualPrivateSourceDisconnectStopsFixedSet(t *testing.T) {
	c, _ := launch(t)
	s, e := Start(context.Background(), c)
	require.NoError(t, e)
	t.Cleanup(func() { _ = s.Stop() })
	requestStatus(t, "http://"+c.Members[0].Listen+"/admit?id=before-disconnect", 200)
	// The source connection, not a process kill, disappears. The child must not
	// keep accepting a still-time-valid cached proof or return to legacy mode.
	require.NoError(t, s.children[0].input.Close())

	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("source loss did not terminate fixed set")
	}

	require.Error(t, s.Stop())

	for _, ch := range s.children {
		select {
		case <-ch.done:
		default:
			t.Fatal("child not joined")
		}
	}

	db, e := model.OpenSQLite(filepath.Join(c.Directory, "runtime.sqlite"))
	require.NoError(t, e)

	defer func() { p, _ := db.DB(); p.Close() }()

	var n int64
	require.NoError(t, db.Model(&model.ChannelAdmissionUse{}).Where("state='open'").Count(&n).Error)
	require.Equal(t, int64(1), n)
}

func TestActualGatewayMissingPrivateSessionDoesNotCreateDatabase(t *testing.T) {
	binary := os.Getenv("B2_CONTROLLED_GATEWAY_BINARY")
	if binary == "" {
		t.Skip("built gateway required")
	}

	dir := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	//nolint:gosec // Runs the explicitly configured local test binary in the controlled child-process fixture.
	cmd := exec.CommandContext(
		ctx,
		binary,
		"--controlled-runtime-read-fd=3",
		"--controlled-runtime-write-fd=4",
	)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"ADMIN_KEY=synthetic-local-admin",
		"SQL_DSN=" + filepath.Join(dir, "must-not-create.sqlite"),
	}
	require.Error(t, cmd.Run())

	entries, e := os.ReadDir(dir)
	require.NoError(t, e)
	require.Empty(t, entries)
}
