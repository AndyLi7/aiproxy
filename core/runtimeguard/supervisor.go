package runtimeguard

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/labring/aiproxy/core/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Member struct {
	ServiceID string `json:"ServiceID"`
	Listen    string `json:"Listen"`
}
type Launch struct {
	Directory              string   `json:"Directory"`
	Seed                   string   `json:"Seed"`
	SeedSHA256             string   `json:"SeedSHA256"`
	Executable             string   `json:"Executable"`
	ExecutableSHA256       string   `json:"ExecutableSHA256"`
	Members                []Member `json:"Members"`
	ValidUntilMs           int64    `json:"ValidUntilMs"`
	RevocationFreshUntilMs int64    `json:"RevocationFreshUntilMs"`
}
type child struct {
	cmd    *exec.Cmd
	input  *os.File
	output *os.File
	read   *bufio.Reader
	hello  Hello
	boot   Boot
	done   chan struct{}
}
type Supervisor struct {
	mu        sync.Mutex
	ledger    *Ledger
	key       ed25519.PrivateKey
	children  []*child
	proof     model.VerifiedDeploymentProof
	namespace string
	closed    bool
	cancel    context.CancelFunc
	done      chan struct{}
	stopErr   error
}

func ownedFile(path string, limit int64) ([]byte, error) {
	st, e := os.Lstat(path)
	if e != nil || !st.Mode().IsRegular() || st.Size() > limit {
		return nil, ErrUntrusted
	}

	if !trustedFileOwner(st) {
		return nil, ErrUntrusted
	}

	return os.ReadFile(path)
}

func writeExclusive(path string, b []byte, mode os.FileMode) error {
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if e != nil {
		return e
	}

	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}

	closeErr := f.Close()

	if e != nil {
		return e
	}

	return closeErr
}

func safeListen(v string) bool {
	if !strings.HasPrefix(v, "127.0.0.1:") {
		return false
	}

	n, e := strconv.Atoi(strings.TrimPrefix(v, "127.0.0.1:"))

	return e == nil && n > 0 && n <= 65535
}

//nolint:gocyclo // Startup orders owned artifact checks, exclusive namespace creation, ledger setup, child bootstrap, and failure cleanup before activation.
func Start(ctx context.Context, c Launch) (s *Supervisor, err error) {
	if len(c.Members) < 1 || len(c.Members) > 16 || !filepath.IsAbs(c.Seed) ||
		!filepath.IsAbs(c.Executable) {
		return nil, ErrUntrusted
	}

	seed, e := ownedFile(c.Seed, 64<<20)
	if e != nil || digest(seed) != c.SeedSHA256 {
		return nil, ErrUntrusted
	}

	for _, suffix := range []string{"-wal", "-journal"} {
		if _, e = os.Lstat(c.Seed + suffix); !os.IsNotExist(e) {
			return nil, ErrUntrusted
		}
	}

	binary, e := ownedFile(c.Executable, 512<<20)
	if e != nil || digest(binary) != c.ExecutableSHA256 {
		return nil, ErrUntrusted
	}

	seen := map[string]bool{}
	for _, m := range c.Members {
		if m.ServiceID == "" || len(m.ServiceID) > 128 || seen[m.ServiceID] || seen[m.Listen] ||
			!safeListen(m.Listen) {
			return nil, ErrUntrusted
		}

		seen[m.ServiceID] = true
		seen[m.Listen] = true
	}

	l, e := NewLedger(c.Directory)
	if e != nil {
		return nil, e
	}

	ctx, cancel := context.WithCancel(ctx)
	s = &Supervisor{ledger: l, cancel: cancel, done: make(chan struct{})}

	c.Directory, e = filepath.EvalSymlinks(c.Directory)
	if e != nil {
		l.Close()
		cancel()
		return nil, e
	}

	owned := s
	defer func() {
		if err != nil {
			_ = owned.Stop()
		}
	}()

	s.namespace, e = randomID()
	if e != nil {
		return nil, e
	}

	epoch, e := randomID()
	if e != nil {
		return nil, e
	}

	public, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		return nil, e
	}

	s.key = key
	if e = writeExclusive(filepath.Join(c.Directory, "issuer.key"), key, 0o600); e != nil {
		return nil, e
	}

	if e = writeExclusive(filepath.Join(c.Directory, "gateway"), binary, 0o700); e != nil {
		return nil, e
	}

	database := filepath.Join(c.Directory, "runtime.sqlite")
	if e = writeExclusive(database, seed, 0o600); e != nil {
		return nil, e
	}

	dbURL := (&url.URL{Scheme: "file", Path: database, RawQuery: "mode=ro"}).String()

	db, e := gorm.Open(sqlite.Open(dbURL), &gorm.Config{Logger: logger.Discard})
	if e != nil {
		return nil, e
	}

	e = model.CheckControlledFreshStorage(db)

	sqlDB, _ := db.DB()
	if sqlDB != nil {
		_ = sqlDB.Close()
	}

	if e != nil {
		return nil, e
	}

	if e = l.Append("fresh_namespace", struct {
		Namespace, Epoch, SeedHash, Artifact string
		Members                              []Member
	}{s.namespace, epoch, c.SeedSHA256, c.ExecutableSHA256, c.Members}); e != nil {
		return nil, e
	}

	for _, m := range c.Members {
		incoming, parentWrite, e := os.Pipe()
		if e != nil {
			return nil, e
		}

		parentRead, outgoing, e := os.Pipe()
		if e != nil {
			incoming.Close()
			parentWrite.Close()
			return nil, e
		}

		closePipes := func() {
			incoming.Close()
			outgoing.Close()
			parentRead.Close()
			parentWrite.Close()
		}

		session, e := randomID()
		if e != nil {
			closePipes()
			return nil, e
		}

		admin, e := randomID()
		if e != nil {
			closePipes()
			return nil, e
		}

		h := Hello{
			Version:      1,
			Session:      session,
			PublicKey:    public,
			ArtifactHash: c.ExecutableSHA256,
			Namespace:    s.namespace,
			ServiceID:    m.ServiceID,
			Epoch:        epoch,
			DatabasePath: database,
			SeedHash:     c.SeedSHA256,
		}

		//nolint:gosec // Runs the exclusive owned artifact copy after checking its bytes against the reviewed executable digest.
		cmd := exec.CommandContext(
			ctx,
			filepath.Join(c.Directory, "gateway"),
			"--controlled-runtime-read-fd=3",
			"--controlled-runtime-write-fd=4",
			"--listen="+m.Listen,
		)
		cmd.Dir = c.Directory
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"ADMIN_KEY=" + admin,
			"DISABLE_AUTO_MIGRATE_DB=true",
		}
		cmd.ExtraFiles = []*os.File{incoming, outgoing}

		log, e := os.OpenFile(
			filepath.Join(c.Directory, "child-"+strconv.Itoa(len(s.children))+".log"),
			os.O_CREATE|os.O_EXCL|os.O_WRONLY,
			0o600,
		)
		if e != nil {
			closePipes()
			return nil, e
		}

		cmd.Stdout = log

		cmd.Stderr = log
		if e = cmd.Start(); e != nil {
			incoming.Close()
			outgoing.Close()
			parentWrite.Close()
			parentRead.Close()
			log.Close()

			return nil, e
		}

		incoming.Close()
		outgoing.Close()

		ch := &child{
			cmd:    cmd,
			input:  parentWrite,
			output: parentRead,
			read:   streamReader(parentRead),
			hello:  h,
			done:   make(chan struct{}),
		}

		s.children = append(s.children, ch)
		go func() { _ = cmd.Wait(); log.Close(); close(ch.done) }()

		_ = parentRead.SetReadDeadline(time.Now().Add(30 * time.Second))

		if e = writeFrame(parentWrite, h); e != nil {
			return nil, e
		}

		if e = readFrame(ch.read, &ch.boot); e != nil {
			return nil, e
		}

		if ch.boot.Version != 1 || ch.boot.Session != session ||
			ch.boot.ArtifactHash != h.ArtifactHash ||
			len(ch.boot.BootIncarnation) != 64 {
			return nil, ErrUntrusted
		}

		var fresh Reply
		if e = readFrame(ch.read, &fresh); e != nil || fresh.Kind != "fresh" {
			return nil, ErrUntrusted
		}
	}

	if e = s.issue(c.ValidUntilMs, c.RevocationFreshUntilMs); e != nil {
		return nil, e
	}

	for _, ch := range s.children {
		go func(ch *child) {
			select {
			case <-ch.done:
			case <-ctx.Done():
			}

			_ = s.Stop()
		}(ch)
	}

	return s, nil
}

func (s *Supervisor) issue(until, fresh int64) error {
	now := time.Now().UnixMilli()
	if until <= now || fresh <= now || fresh > until {
		return ErrUntrusted
	}

	p := s.proof

	var idErr error

	p.ProofID, idErr = randomID()
	if idErr != nil {
		return idErr
	}

	p.Epoch = s.children[0].hello.Epoch
	p.Sequence++
	p.StorageGeneration = s.namespace
	p.StorageWatermark = 0
	p.IssuedAtMs = now
	p.ValidUntilMs = until

	p.RevocationFreshUntilMs = fresh
	if len(p.Executors) == 0 {
		for _, c := range s.children {
			p.Executors = append(
				p.Executors,
				model.ChannelExecutorIdentity{
					ServiceID:  c.hello.ServiceID,
					InstanceID: c.boot.BootIncarnation,
					BuildSHA:   c.hello.ArtifactHash,
					Epoch:      p.Epoch,
				},
			)
		}

		var hashErr error

		p.MembershipHash, hashErr = hashValue(p.Executors)
		if hashErr != nil {
			return hashErr
		}

		p.ExecutorFenceHash, hashErr = hashValue(
			[]string{"local-supervised-fresh-v1", s.namespace, p.MembershipHash},
		)
		if hashErr != nil {
			return hashErr
		}

		if hashErr != nil {
			return hashErr
		}
	}

	if e := s.ledger.Append("proof_issued", p); e != nil {
		return e
	}
	// Persist before sending any permission. A partial activation is unknown and
	// closes the whole local session; it never silently retries a changed proof.
	s.proof = p
	for _, ch := range s.children {
		if e := s.send(ch, "activate"); e != nil {
			return e
		}
	}

	return nil
}

func (s *Supervisor) send(ch *child, kind string) error {
	select {
	case <-ch.done:
		return ErrUntrusted
	default:
	}

	_ = ch.output.SetReadDeadline(time.Now().Add(30 * time.Second))

	c := Command{1, kind, ch.hello.Session, ch.boot.BootIncarnation, s.namespace, s.proof}

	signed, err := sign(s.key, c)
	if err != nil {
		return err
	}

	if e := writeFrame(ch.input, signed); e != nil {
		return e
	}

	var r Reply
	if e := readFrame(ch.read, &r); e != nil {
		return e
	}

	expected := "active"
	if kind == "revoke" {
		expected = "revoked"
	}

	if r.Kind != expected || r.Sequence != s.proof.Sequence || r.Error != "" {
		return ErrUntrusted
	}

	return nil
}

func (s *Supervisor) Renew(until, fresh int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrUntrusted
	}

	if e := s.issue(until, fresh); e != nil {
		s.cancel()
		return e
	}

	return nil
}

func (s *Supervisor) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return s.stopErr
	}

	s.closed = true
	defer close(s.done)

	var e error
	if s.proof.Sequence > 0 {
		e = s.ledger.Append("proof_revoked", s.proof)
		if e == nil {
			for _, ch := range s.children {
				if x := s.send(ch, "revoke"); x != nil {
					e = errors.Join(e, x)
				}
			}
		}
	}

	s.cancel()

	for _, ch := range s.children {
		ch.input.Close()
		ch.output.Close()

		select {
		case <-ch.done:
		case <-time.After(30 * time.Second):
			e = errors.Join(e, ErrUntrusted)
		}
	}

	if e == nil {
		e = s.ledger.Append("runtime_stopped", struct{ Namespace string }{s.namespace})
	}

	s.stopErr = errors.Join(e, s.ledger.Close())

	return s.stopErr
}

func (s *Supervisor) Done() <-chan struct{} { return s.done }

// OpenControlledStorage consumes only the parent-authenticated private bootstrap
// path, never SQL_DSN or ordinary environment fallbacks. mode=rw cannot create a
// missing database. The exact seed bytes are checked before the first connection.
func OpenControlledStorage(h Hello) (*gorm.DB, error) {
	cwd, e := os.Getwd()
	if e != nil || h.DatabasePath != filepath.Join(cwd, "runtime.sqlite") {
		return nil, ErrUntrusted
	}

	raw, e := ownedFile(h.DatabasePath, 64<<20)
	if e != nil || digest(raw) != h.SeedHash {
		return nil, ErrUntrusted
	}

	dbURL := (&url.URL{Scheme: "file", Path: h.DatabasePath, RawQuery: "mode=rw&_pragma=busy_timeout(5000)"}).String()

	db, e := gorm.Open(sqlite.Open(dbURL), &gorm.Config{Logger: logger.Discard})
	if e != nil {
		return nil, e
	}

	if e = model.CheckControlledFreshStorage(db); e != nil {
		pool, _ := db.DB()
		if pool != nil {
			pool.Close()
		}

		return nil, e
	}

	return db, nil
}
