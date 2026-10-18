package runtimeguard

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func signedFixture(t *testing.T) (*Verifier, Command, ed25519.PrivateKey) {
	t.Helper()
	pub, key, e := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, e)
	h := Hello{Version: 1, Session: strings.Repeat("a", 64), PublicKey: pub, ArtifactHash: strings.Repeat("b", 64), Namespace: "local-test-namespace", ServiceID: "gateway", Epoch: "test-epoch"}
	boot := strings.Repeat("c", 64)
	v, e := NewVerifier(h, boot)
	require.NoError(t, e)
	now := time.Now()
	p := model.VerifiedDeploymentProof{ProofID: strings.Repeat("d", 64), Epoch: h.Epoch, Sequence: 1, StorageGeneration: h.Namespace, ExecutorFenceHash: strings.Repeat("e", 64), IssuedAtMs: now.Add(-time.Second).UnixMilli(), ValidUntilMs: now.Add(time.Minute).UnixMilli(), RevocationFreshUntilMs: now.Add(time.Minute).UnixMilli(), Executors: []model.ChannelExecutorIdentity{{ServiceID: h.ServiceID, InstanceID: boot, BuildSHA: h.ArtifactHash, Epoch: h.Epoch}}}
	p.MembershipHash = hashValue(p.Executors)
	return v, Command{1, "activate", h.Session, boot, h.Namespace, p}, key
}
func TestSignedPrivateBootScope(t *testing.T) {
	for _, mode := range []string{"valid", "other-key", "old-boot", "old-session", "other-namespace", "tampered", "same-proof-extended", "sequence-rollback", "member-changed", "expiry", "generation", "watermark"} {
		t.Run(mode, func(t *testing.T) {
			v, c, key := signedFixture(t)
			if mode == "same-proof-extended" || mode == "sequence-rollback" || mode == "member-changed" {
				require.NoError(t, v.stage(c))
			}
			switch mode {
			case "other-key":
				_, key, _ = ed25519.GenerateKey(rand.Reader)
			case "old-boot":
				c.Boot = strings.Repeat("f", 64)
			case "old-session":
				c.Session = strings.Repeat("f", 64)
			case "other-namespace":
				c.Namespace = "other"
			case "same-proof-extended":
				c.Proof.ValidUntilMs++
				c.Proof.Sequence++
			case "sequence-rollback":
				c.Proof.ProofID = strings.Repeat("f", 64)
			case "member-changed":
				c.Proof.Sequence++
				c.Proof.ProofID = strings.Repeat("f", 64)
				c.Proof.Executors = append(c.Proof.Executors, model.ChannelExecutorIdentity{ServiceID: "rogue", InstanceID: strings.Repeat("f", 64), BuildSHA: strings.Repeat("b", 64), Epoch: c.Proof.Epoch})
				c.Proof.MembershipHash = hashValue(c.Proof.Executors)
			case "expiry":
				c.Proof.ValidUntilMs = time.Now().Add(-time.Second).UnixMilli()
			case "generation":
				c.Proof.StorageGeneration = "other"
			case "watermark":
				c.Proof.StorageWatermark = 1
			}
			signed := sign(key, c)
			if mode == "tampered" {
				signed.Payload[0] = 'x'
			}
			parsed, e := v.Verify(signed)
			if e == nil {
				e = v.stage(parsed)
			}
			if mode == "valid" {
				require.NoError(t, e)
				_, e = v.Load(context.Background())
				require.NoError(t, e)
			} else {
				require.Error(t, e)
			}
		})
	}
}
func TestLedgerIsDurableNotRestoreAuthority(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	l, e := NewLedger(dir)
	require.NoError(t, e)
	require.NoError(t, l.Append("proof_issued", map[string]string{"id": "synthetic"}))
	require.NoError(t, l.Append("proof_revoked", map[string]string{"id": "synthetic"}))
	require.NoError(t, l.Close())
	raw, e := os.ReadFile(filepath.Join(dir, "issuance.log"))
	require.NoError(t, e)
	require.NoError(t, VerifyLedger(bytes.NewReader(raw)))
	require.Error(t, VerifyLedger(bytes.NewReader(raw[:len(raw)-1])))
	mut := append([]byte(nil), raw...)
	mut[12] = 'x'
	require.Error(t, VerifyLedger(bytes.NewReader(mut)))
	_, e = NewLedger(dir)
	require.Error(t, e, "old session cannot be reissued after DB prefix restore")
}
func TestProtocolFramesBounded(t *testing.T) {
	var x any
	require.Error(t, readFrame(bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", maxFrame+2)), maxFrame+1), &x))
	b, _ := json.Marshal(map[string]int{"a": 1})
	require.Error(t, readFrame(streamReader(bytes.NewReader(b)), &x))
	require.NoError(t, readFrame(streamReader(bytes.NewReader(append(b, '\n'))), &x))
}

func TestKnownRefusalCannotBecomeSourceOutage(t *testing.T) {
	for _, refusal := range []error{model.ErrChannelAdmissionConflict, model.ErrChannelAdmissionSealed} {
		v, c, _ := signedFixture(t)
		require.NoError(t, v.stage(c))
		v.invalidate(refusal)
		v.invalidate(ErrUntrusted)
		proof, err := v.Load(t.Context())
		require.ErrorIs(t, err, refusal)
		require.Equal(t, c.Proof, proof)
		c.Proof.Sequence++
		c.Proof.ProofID = strings.Repeat("f", 64)
		require.ErrorIs(t, v.stage(c), refusal)
	}
}
func TestRunChildFailedPersistentRevokeStillRejectsFrozenNative(t *testing.T) {
	v, c, key := signedFixture(t)
	path := seed(t)
	db, err := model.OpenSQLite(path)
	require.NoError(t, err)
	oldDB, oldLog := model.DB, model.LogDB
	restore := model.ConfigureChannelAdmissionRuntime(model.ChannelExecutorIdentity{}, nil)
	model.DB, model.LogDB = db, db
	defer func() { restore(); model.DB, model.LogDB = oldDB, oldLog; p, _ := db.DB(); p.Close() }()
	require.NoError(t, db.Create(&model.ChannelAdmissionExecutionProtocol{ID: 1, Version: 1}).Error)
	require.NoError(t, db.Create(&model.ChannelAdmissionExecutionProtocol{ID: 2, Version: model.NativeExecutionProtocolVersion}).Error)
	in, parentWrite, err := os.Pipe()
	require.NoError(t, err)
	defer parentWrite.Close()
	parentRead, out, err := os.Pipe()
	require.NoError(t, err)
	defer parentRead.Close()
	defer out.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	ready := make(chan error, 1)
	go func() { done <- RunChild(ctx, v.hello, streamReader(in), in, out, v.boot, ready) }()
	reader := streamReader(parentRead)
	var reply Reply
	require.NoError(t, readFrame(reader, &reply))
	require.Equal(t, "fresh", reply.Kind)
	require.NoError(t, writeFrame(parentWrite, sign(key, c)))
	require.NoError(t, readFrame(reader, &reply))
	require.Equal(t, "active", reply.Kind)
	require.NoError(t, <-ready)
	var channel model.Channel
	require.NoError(t, db.First(&channel, 1).Error)
	identity, err := model.ChannelIdentityOf(&channel)
	require.NoError(t, err)
	task, created, err := model.ReserveNativeTask(db, model.NativeTask{ID: "frozen-before-revoke", GroupID: "group", TokenID: 1, Model: "synthetic/native", Fingerprint: strings.Repeat("a", 64), OutputSchema: `{"type":"object"}`, ChannelID: identity.ChannelID, KeyFingerprint: identity.KeyDigest, AdmissionIdentityHash: identity.IdentityHash})
	require.NoError(t, err)
	require.True(t, created)
	// Establish that the actual no-anchor frozen compatibility branch accepts
	// a genuine zero-proof outage, without creating an execution anchor.
	before, err := model.MigrationProbeRuntimeStore(db, db, nil)
	require.NoError(t, err)
	restoreOutage := model.ConfigureChannelAdmissionRuntime(before.Executor, func(context.Context) (model.VerifiedDeploymentProof, error) {
		return model.VerifiedDeploymentProof{}, ErrUntrusted
	})
	h, err := model.BeginNativeTaskExecution(t.Context(), db, task, model.NativeExecutionPoll, "")
	restoreOutage()
	require.NoError(t, err)
	require.Nil(t, h)
	require.NoError(t, db.Exec("CREATE TRIGGER refuse_proof_revoke BEFORE UPDATE OF revoked ON channel_admission_proofs BEGIN SELECT RAISE(ABORT, 'synthetic revoke failure'); END").Error)
	c.Kind = "revoke"
	require.NoError(t, writeFrame(parentWrite, sign(key, c)))
	select {
	case err = <-done:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("revoke did not return")
	}
	var stored model.ChannelAdmissionProof
	require.NoError(t, db.First(&stored, "id=?", c.Proof.ProofID).Error)
	require.False(t, stored.Revoked)
	store, err := model.MigrationProbeRuntimeStore(db, db, nil)
	require.NoError(t, err)
	proof, err := store.LoadDeployment(t.Context())
	require.ErrorIs(t, err, model.ErrChannelAdmissionConflict)
	require.Equal(t, c.Proof, proof)
	h, err = model.BeginNativeTaskExecution(t.Context(), db, task, model.NativeExecutionPoll, "")
	require.Error(t, err)
	require.Nil(t, h)
	var count int64
	require.NoError(t, db.Model(&model.ChannelAdmissionTaskExecution{}).Count(&count).Error)
	require.Zero(t, count)
}
