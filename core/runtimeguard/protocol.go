// Package runtimeguard implements ONLY a local, supervisor-owned fixed process
// set. Its signatures never attest Railway, an unknown fleet or recovery safety.
package runtimeguard

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/labring/aiproxy/core/model"
)

const maxFrame = 256 * 1024

var ErrUntrusted = errors.New("controlled runtime unavailable")

func randomID() (string, error) {
	var b [32]byte
	_, e := rand.Read(b[:])
	return hex.EncodeToString(b[:]), e
}
func digest(v []byte) string { h := sha256.Sum256(v); return hex.EncodeToString(h[:]) }
func hashValue(v any) string { b, _ := json.Marshal(v); return digest(b) }

type Hello struct {
	DatabasePath string
	SeedHash     string
	Version      int
	Session      string
	PublicKey    []byte
	ArtifactHash string
	Namespace    string
	ServiceID    string
	Epoch        string
}
type Boot struct {
	Version         int
	Session         string
	BootIncarnation string
	ArtifactHash    string
}
type Signed struct {
	Payload   json.RawMessage
	Signature []byte
}
type Command struct {
	Version   int
	Kind      string
	Session   string
	Boot      string
	Namespace string
	Proof     model.VerifiedDeploymentProof
}
type Reply struct {
	Kind     string
	Sequence int64
	Error    string
}

func sign(key ed25519.PrivateKey, c Command) Signed {
	raw, _ := json.Marshal(c)
	return Signed{raw, ed25519.Sign(key, raw)}
}
func readFrame(r *bufio.Reader, v any) error {
	raw, e := r.ReadSlice('\n')
	if e != nil {
		if e == io.EOF && len(raw) > 0 {
			return ErrUntrusted
		}
		return e
	}
	if len(raw) > maxFrame {
		return ErrUntrusted
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return ErrUntrusted
	}
	if d.Decode(new(any)) != io.EOF {
		return ErrUntrusted
	}
	return nil
}
func streamReader(r io.Reader) *bufio.Reader { return bufio.NewReaderSize(r, maxFrame+1) }
func writeFrame(w io.Writer, v any) error {
	b, e := json.Marshal(v)
	if e != nil || len(b) > maxFrame {
		return ErrUntrusted
	}
	b = append(b, '\n')
	_, e = w.Write(b)
	return e
}

type Verifier struct {
	mu    sync.RWMutex
	hello Hello
	boot  string
	proof model.VerifiedDeploymentProof
	err   error
}

func NewVerifier(h Hello, boot string) (*Verifier, error) {
	if h.Version != 1 || len(h.PublicKey) != ed25519.PublicKeySize || len(h.Session) != 64 || len(boot) != 64 || h.Namespace == "" || h.ServiceID == "" || len(h.ArtifactHash) != 64 || h.Epoch == "" {
		return nil, ErrUntrusted
	}
	return &Verifier{hello: h, boot: boot, err: ErrUntrusted}, nil
}
func (v *Verifier) Verify(s Signed) (Command, error) {
	var c Command
	if len(s.Payload) > maxFrame || !ed25519.Verify(v.hello.PublicKey, s.Payload, s.Signature) {
		return c, ErrUntrusted
	}
	d := json.NewDecoder(bytes.NewReader(s.Payload))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return c, ErrUntrusted
	}
	if c.Version != 1 || c.Session != v.hello.Session || c.Boot != v.boot || c.Namespace != v.hello.Namespace {
		return c, ErrUntrusted
	}
	return c, nil
}
func (v *Verifier) stage(c Command) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	p := c.Proof
	if terminalRefusal(v.err) {
		return v.err
	}
	if c.Kind != "activate" || p.Epoch != v.hello.Epoch || p.StorageGeneration != v.hello.Namespace || p.StorageWatermark != 0 || p.Sequence <= v.proof.Sequence || p.ProofID == v.proof.ProofID || len(p.Executors) == 0 || len(p.Executors) > 16 || p.MembershipHash != hashValue(p.Executors) {
		return ErrUntrusted
	}
	if v.proof.Sequence > 0 && (p.MembershipHash != v.proof.MembershipHash || p.ExecutorFenceHash != v.proof.ExecutorFenceHash) {
		return ErrUntrusted
	}
	found := false
	seen := map[string]bool{}
	for _, x := range p.Executors {
		if seen[x.InstanceID] || x.Epoch != p.Epoch || len(x.InstanceID) != 64 || len(x.BuildSHA) != 64 || x.ServiceID == "" {
			return ErrUntrusted
		}
		seen[x.InstanceID] = true
		if x.InstanceID == v.boot && x.BuildSHA == v.hello.ArtifactHash && x.ServiceID == v.hello.ServiceID {
			found = true
		}
	}
	now := time.Now().UnixMilli()
	if !found || p.IssuedAtMs <= 0 || p.IssuedAtMs > now || p.ValidUntilMs <= now || p.RevocationFreshUntilMs <= now || p.RevocationFreshUntilMs > p.ValidUntilMs || len(p.ProofID) != 64 || len(p.ExecutorFenceHash) != 64 {
		return ErrUntrusted
	}
	v.proof = p
	v.proof.Executors = append([]model.ChannelExecutorIdentity(nil), p.Executors...)
	v.err = nil
	return nil
}
func (v *Verifier) Load(ctx context.Context) (model.VerifiedDeploymentProof, error) {
	if e := ctx.Err(); e != nil {
		return model.VerifiedDeploymentProof{}, e
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	p := v.proof
	p.Executors = append([]model.ChannelExecutorIdentity(nil), p.Executors...)
	if v.err != nil {
		if !terminalRefusal(v.err) {
			return model.VerifiedDeploymentProof{}, v.err
		}
		return p, v.err
	}
	now := time.Now().UnixMilli()
	if now < p.IssuedAtMs || now >= p.ValidUntilMs || now >= p.RevocationFreshUntilMs {
		return model.VerifiedDeploymentProof{}, ErrUntrusted
	}
	return p, nil
}
func terminalRefusal(e error) bool {
	return errors.Is(e, model.ErrChannelAdmissionConflict) || errors.Is(e, model.ErrChannelAdmissionSealed)
}
func (v *Verifier) invalidate(e error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	// A known revocation cannot degrade into generic source outage, even if its
	// persistent write fails. Frozen-work compatibility distinguishes these facts.
	if !terminalRefusal(v.err) {
		v.err = e
	}
}

// ChildHandshake runs before DB initialization. Both FDs must be inherited
// private pipes owned by the trusted local launcher; no HTTP/env identity source.
func ChildHandshake(in, out *os.File) (Hello, *bufio.Reader, string, error) {
	var h Hello
	if in == nil || out == nil {
		return h, nil, "", ErrUntrusted
	}
	for _, f := range []*os.File{in, out} {
		st, e := f.Stat()
		if e != nil || st.Mode()&os.ModeNamedPipe == 0 {
			return h, nil, "", ErrUntrusted
		}
	}
	_ = in.SetReadDeadline(time.Now().Add(30 * time.Second))
	r := streamReader(in)
	if e := readFrame(r, &h); e != nil {
		return h, nil, "", ErrUntrusted
	}
	exe, e := os.Executable()
	if e != nil {
		return h, nil, "", e
	}
	raw, e := os.ReadFile(exe)
	if e != nil || digest(raw) != h.ArtifactHash {
		return h, nil, "", ErrUntrusted
	}
	boot, e := model.ProcessBootIncarnation()
	if e != nil {
		return h, nil, "", e
	}
	if _, e = NewVerifier(h, boot); e != nil {
		return h, nil, "", e
	}
	if e = writeFrame(out, Boot{1, h.Session, boot, digest(raw)}); e != nil {
		return h, nil, "", e
	}
	return h, r, boot, nil
}

// RunChild installs the actual model runtime, then consumes signed commands.
// ready closes ONLY after fresh validation and Activate. EOF/invalid commands
// revoke the cached source and stop the child; they never restore legacy mode.
func RunChild(ctx context.Context, h Hello, r *bufio.Reader, in, out *os.File, boot string, ready chan<- error) error {
	v, e := NewVerifier(h, boot)
	if e != nil {
		ready <- e
		return e
	}
	if e = model.CheckControlledFreshStorage(model.DB); e != nil {
		ready <- e
		return e
	}
	s, e := model.InstallControlledAdmissionRuntime(model.ChannelExecutorIdentity{ServiceID: h.ServiceID, InstanceID: boot, BuildSHA: h.ArtifactHash, Epoch: h.Epoch}, v.Load)
	if e != nil {
		ready <- e
		return e
	}
	if e = writeFrame(out, Reply{Kind: "fresh"}); e != nil {
		ready <- e
		return e
	}
	first := true
	var seq int64
	defer v.invalidate(ErrUntrusted)
	defer func() {
		if first {
			ready <- ErrUntrusted
		}
	}()
	readerCtx, cancelReader := context.WithCancel(ctx)
	defer cancelReader()
	go func() { <-readerCtx.Done(); _ = in.Close() }()
	for {
		var signed Signed
		if e = readFrame(r, &signed); e != nil {
			return ErrUntrusted
		}
		c, e := v.Verify(signed)
		if e != nil {
			return e
		}
		switch c.Kind {
		case "activate":
			if e = v.stage(c); e == nil {
				_, e = s.Activate(ctx, seq)
			}
			if e != nil {
				_ = writeFrame(out, Reply{Error: "activation_refused"})
				return ErrUntrusted
			}
			seq = c.Proof.Sequence
			_ = in.SetReadDeadline(time.Time{})
			if e = writeFrame(out, Reply{Kind: "active", Sequence: seq}); e != nil {
				return e
			}
			if first {
				first = false
				ready <- nil
			}
		case "revoke":
			v.mu.RLock()
			p := v.proof
			v.mu.RUnlock()
			if c.Proof.ProofID != p.ProofID || hashValue(c.Proof) != hashValue(p) {
				return ErrUntrusted
			}
			v.invalidate(model.ErrChannelAdmissionConflict)
			hash, _ := p.Fingerprint()
			if e = model.RevokeDeploymentProof(ctx, model.DB, p.ProofID, hash); e != nil {
				return e
			}
			if e = writeFrame(out, Reply{Kind: "revoked", Sequence: seq}); e != nil {
				return e
			}
			return nil
		default:
			return fmt.Errorf("%w: command", ErrUntrusted)
		}
	}
}
