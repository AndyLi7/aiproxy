package model_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type admissionFixture struct {
	store      *model.ChannelAdmissionStore
	path       string
	identities []model.ChannelIdentity
	proof      model.VerifiedDeploymentProof
}

func newAdmissionFixture(t *testing.T) *admissionFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admission.db")
	db, err := model.OpenSQLite(path)
	require.NoError(t, err)
	t.Cleanup(func() { pool, e := db.DB(); require.NoError(t, e); require.NoError(t, pool.Close()) })
	return admissionFixtureOn(t, db, path)
}

func admissionFixtureOn(t *testing.T, db *gorm.DB, path string) *admissionFixture {
	t.Helper()
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.ChannelAdmissionControl{}, &model.ChannelAdmissionProof{}, &model.ChannelAdmissionFamily{}, &model.ChannelAdmissionBarrier{}, &model.ChannelAdmissionUse{}))
	proof := model.VerifiedDeploymentProof{ProofID: "synthetic-proof", Epoch: "synthetic-epoch", Sequence: 1, StorageGeneration: "synthetic-generation", MembershipHash: strings.Repeat("c", 64), ExecutorFenceHash: strings.Repeat("d", 64), IssuedAtMs: time.Now().Add(-time.Minute).UnixMilli(), ValidUntilMs: time.Now().Add(time.Minute).UnixMilli(), RevocationFreshUntilMs: time.Now().Add(time.Minute).UnixMilli()}
	executor := model.ChannelExecutorIdentity{ServiceID: "synthetic-gateway", InstanceID: "synthetic-instance", BuildSHA: strings.Repeat("f", 64), Epoch: proof.Epoch}
	proof.Executors = []model.ChannelExecutorIdentity{executor}
	encoded, err := json.Marshal(proof.Executors)
	require.NoError(t, err)
	digest := sha256.Sum256(encoded)
	proof.MembershipHash = hex.EncodeToString(digest[:])
	require.NoError(t, db.Create(&model.ChannelAdmissionControl{ID: 1, Version: 1, Epoch: proof.Epoch, EpochSequence: proof.Sequence, StorageGeneration: proof.StorageGeneration, MembershipHash: proof.MembershipHash, ExecutorFenceHash: proof.ExecutorFenceHash, ActiveProofID: proof.ProofID}).Error)
	hash, err := proof.Fingerprint()
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.ChannelAdmissionProof{ID: proof.ProofID, FactHash: hash, ConsumedSequence: proof.Sequence}).Error)
	f := &admissionFixture{path: path, proof: proof, store: &model.ChannelAdmissionStore{DB: db, LogDB: db, Executor: executor}}
	f.store.LoadDeployment = func(context.Context) (model.VerifiedDeploymentProof, error) { return f.proof, nil }

	for i, key := range []string{"synthetic-old", "synthetic-old", "synthetic-new", "synthetic-old", "synthetic-newer"} {
		channel := model.Channel{ID: i + 1, Name: "synthetic", Key: key, Type: model.ChannelTypeFal, Status: model.ChannelStatusEnabled, BaseURL: "https://example.invalid/" + strings.Repeat("x", i)}
		require.NoError(t, db.Create(&channel).Error)
		identity, err := model.ChannelIdentityOf(&channel)
		require.NoError(t, err)
		f.identities = append(f.identities, identity)
	}
	return f
}
func admissionRequest(id string, identity model.ChannelIdentity) model.ChannelAdmissionRequest {
	return model.ChannelAdmissionRequest{RequestID: id, GroupID: "group", TokenID: 1, Lane: "native", RequestHash: strings.Repeat("a", 64), Identity: identity}
}
func (f *admissionFixture) seal(id string, revision int64, from, to int, revert string) model.ChannelSealRequest {
	return model.ChannelSealRequest{OperationID: id, Epoch: "synthetic-epoch", ExpectedRevision: revision, From: f.identities[from-1], To: f.identities[to-1], RevertOf: revert}
}

func TestAdmissionSameKeySealPreservesFrozenUseAndBlocksAliases(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := context.Background()
	original, created, err := f.store.Admit(ctx, admissionRequest("frozen", f.identities[0]), nil)
	require.NoError(t, err)
	require.True(t, created)
	barrier, err := f.store.Seal(ctx, f.seal("move", 0, 1, 2, ""))
	require.NoError(t, err)
	require.Equal(t, int64(1), barrier.Revision)
	for _, lane := range []string{"native", "image", "relay", "trial", "probe"} {
		for _, index := range []int{0, 3} {
			input := admissionRequest(lane, f.identities[index])
			input.Lane = lane
			_, _, err = f.store.Admit(ctx, input, nil)
			require.ErrorIs(t, err, model.ErrChannelAdmissionSealed)
		}
	}
	_, created, err = f.store.Admit(ctx, admissionRequest("replacement", f.identities[1]), nil)
	require.NoError(t, err)
	require.True(t, created)
	// New same-digest aliases created after sealing receive no successor grant.
	alias := model.Channel{ID: 99, Key: "synthetic-old", Type: model.ChannelTypeFal, Status: model.ChannelStatusEnabled}
	require.NoError(t, f.store.DB.Create(&alias).Error)
	aliasIdentity, err := model.ChannelIdentityOf(&alias)
	require.NoError(t, err)
	_, _, err = f.store.Admit(ctx, admissionRequest("new-alias", aliasIdentity), nil)
	require.ErrorIs(t, err, model.ErrChannelAdmissionSealed)
	// Expired deployment evidence cannot erase an already durable admission.
	f.store.LoadDeployment = func(context.Context) (model.VerifiedDeploymentProof, error) {
		t.Fatal("existing task must not wait for external proof")
		return model.VerifiedDeploymentProof{}, nil
	}
	replay, created, err := f.store.Admit(ctx, admissionRequest("frozen", f.identities[0]), func(*gorm.DB) error { t.Fatal("must not repeat reservation"); return nil })
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, original, replay)
	f.store.LoadDeployment = nil
	_, _, err = f.store.Admit(ctx, admissionRequest("unproven-new", f.identities[1]), nil)
	require.ErrorIs(t, err, model.ErrChannelAdmissionUnavailable)
}

func TestAdmissionContinuousMigrationsAndRevertKeepExactSuccessor(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := context.Background()
	first, err := f.store.Seal(ctx, f.seal("first", 0, 1, 2, ""))
	require.NoError(t, err)
	_, err = f.store.Seal(ctx, f.seal("second", 1, 2, 4, ""))
	require.NoError(t, err)
	for _, index := range []int{0, 1} {
		_, _, err = f.store.Admit(ctx, admissionRequest("old", f.identities[index]), nil)
		require.ErrorIs(t, err, model.ErrChannelAdmissionSealed)
	}
	_, _, err = f.store.Admit(ctx, admissionRequest("current", f.identities[3]), nil)
	require.NoError(t, err)
	_, err = f.store.Seal(ctx, f.seal("revert-second", 2, 4, 2, "second"))
	require.NoError(t, err)
	_, _, err = f.store.Admit(ctx, admissionRequest("reverted", f.identities[1]), nil)
	require.NoError(t, err)
	_, _, err = f.store.Admit(ctx, admissionRequest("retired-again", f.identities[3]), nil)
	require.ErrorIs(t, err, model.ErrChannelAdmissionSealed)
	replay, err := f.store.Seal(ctx, f.seal("first", 0, 1, 2, ""))
	require.NoError(t, err)
	require.Equal(t, first, replay, "stale exact replay returns its result without changing grants")
	_, err = f.store.Seal(ctx, f.seal("first", 3, 2, 1, "first"))
	require.ErrorIs(t, err, model.ErrChannelAdmissionConflict)
}

func TestAdmissionKeyRotationAndRevertDoNotOpenAliasFamily(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := context.Background()
	_, err := f.store.Seal(ctx, f.seal("rotate", 0, 1, 3, ""))
	require.NoError(t, err)
	for _, index := range []int{0, 1, 3} {
		_, _, err = f.store.Admit(ctx, admissionRequest("blocked", f.identities[index]), nil)
		require.ErrorIs(t, err, model.ErrChannelAdmissionSealed)
	}
	_, _, err = f.store.Admit(ctx, admissionRequest("new", f.identities[2]), nil)
	require.NoError(t, err)
	_, err = f.store.Seal(ctx, f.seal("fake-revert", 1, 3, 2, "rotate"))
	require.ErrorIs(t, err, model.ErrChannelAdmissionConflict)
	_, err = f.store.Seal(ctx, f.seal("revert", 1, 3, 1, "rotate"))
	require.NoError(t, err)
	_, _, err = f.store.Admit(ctx, admissionRequest("restored", f.identities[0]), nil)
	require.NoError(t, err)
	for _, index := range []int{1, 2, 3} {
		_, _, err = f.store.Admit(ctx, admissionRequest("still-blocked", f.identities[index]), nil)
		require.ErrorIs(t, err, model.ErrChannelAdmissionSealed)
	}
}

func TestAdmissionReservationFailureRollsBackUseAndSealFailureRollsBackFamily(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := context.Background()
	failure := errors.New("synthetic reservation failure")
	_, created, err := f.store.Admit(ctx, admissionRequest("failed", f.identities[0]), func(tx *gorm.DB) error {
		require.NoError(t, tx.Model(&model.Channel{}).Where("id = ?", 1).Update("remark", "must roll back").Error)
		return failure
	})
	require.ErrorIs(t, err, failure)
	require.False(t, created)
	var uses int64
	require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionUse{}).Count(&uses).Error)
	require.Zero(t, uses)
	var channel model.Channel
	require.NoError(t, f.store.DB.First(&channel, 1).Error)
	require.Empty(t, channel.Remark)
	require.NoError(t, f.store.DB.Exec("CREATE TRIGGER reject_barrier BEFORE INSERT ON channel_admission_barriers BEGIN SELECT RAISE(ABORT, 'synthetic barrier failure'); END").Error)
	_, err = f.store.Seal(ctx, f.seal("failed-seal", 0, 1, 2, ""))
	require.Error(t, err)
	var families int64
	require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionFamily{}).Count(&families).Error)
	require.Zero(t, families)
	var control model.ChannelAdmissionControl
	require.NoError(t, f.store.DB.First(&control, 1).Error)
	require.Zero(t, control.Revision)
}

func TestAdmissionRequiresProofCurrentIdentityVersionAndCompleteStorage(t *testing.T) {
	for _, failure := range []string{"no-proof", "split-db", "wrong-version", "missing-table", "identity-drift", "wrong-epoch", "stale-revision"} {
		t.Run(failure, func(t *testing.T) {
			f := newAdmissionFixture(t)
			request := f.seal("seal", 0, 1, 2, "")
			switch failure {
			case "no-proof":
				f.store.LoadDeployment = nil
			case "split-db":
				f.store.LogDB = f.store.DB.Session(&gorm.Session{})
			case "wrong-version":
				require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionControl{}).Where("id = ?", 1).Update("version", 2).Error)
			case "missing-table":
				require.NoError(t, f.store.DB.Migrator().DropTable(&model.ChannelAdmissionUse{}))
			case "identity-drift":
				require.NoError(t, f.store.DB.Model(&model.Channel{}).Where("id = ?", 1).Update("base_url", "https://drift.invalid").Error)
			case "wrong-epoch":
				request.Epoch = "old-epoch"
			case "stale-revision":
				request.ExpectedRevision = 1
			}
			_, err := f.store.Seal(context.Background(), request)
			require.Error(t, err)
			var count int64
			require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionBarrier{}).Count(&count).Error)
			require.Zero(t, count)
			require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionFamily{}).Count(&count).Error)
			require.Zero(t, count)
		})
	}
}

func TestAdmissionAndSealSerializeAcrossConnectionsAndReopen(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := context.Background()
	second, err := model.OpenSQLite(f.path)
	require.NoError(t, err)
	pool, err := second.DB()
	require.NoError(t, err)
	defer pool.Close()
	other := &model.ChannelAdmissionStore{DB: second, LogDB: second, Executor: f.store.Executor, LoadDeployment: f.store.LoadDeployment}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	var admitted *model.ChannelAdmissionUse
	var admitErr, sealErr error
	go func() {
		defer wg.Done()
		<-start
		admitted, _, admitErr = f.store.Admit(ctx, admissionRequest("racing", f.identities[0]), nil)
	}()
	go func() { defer wg.Done(); <-start; _, sealErr = other.Seal(ctx, f.seal("seal", 0, 1, 2, "")) }()
	close(start)
	wg.Wait()
	require.NoError(t, sealErr)
	if admitErr != nil {
		require.ErrorIs(t, admitErr, model.ErrChannelAdmissionSealed)
		require.Nil(t, admitted)
	} else {
		require.NotNil(t, admitted)
	}
	require.NoError(t, pool.Close())
	reopened, err := model.OpenSQLite(f.path)
	require.NoError(t, err)
	reopenedPool, err := reopened.DB()
	require.NoError(t, err)
	defer reopenedPool.Close()
	restored := &model.ChannelAdmissionStore{DB: reopened, LogDB: reopened, Executor: f.store.Executor, LoadDeployment: f.store.LoadDeployment}
	_, _, err = restored.Admit(ctx, admissionRequest("after-reopen", f.identities[0]), nil)
	require.ErrorIs(t, err, model.ErrChannelAdmissionSealed)
	if admitted != nil {
		replay, created, err := restored.Admit(ctx, admissionRequest("racing", f.identities[0]), nil)
		require.NoError(t, err)
		require.False(t, created)
		require.Equal(t, admitted, replay)
	}
}

func TestAdmissionMigrationDraftIsAdditiveAndDoesNotActivate(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "draft.db"))
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	defer pool.Close()
	script, err := os.ReadFile("../migrations/channel-admission-v1/sqlite.sql")
	require.NoError(t, err)
	for range 2 {
		require.NoError(t, db.Exec(string(script)).Error)
	}
	require.True(t, model.ChannelAdmissionStorageReady(db))
	var control model.ChannelAdmissionControl
	require.NoError(t, db.First(&control, 1).Error)
	require.Empty(t, control.Epoch)
	require.Zero(t, control.Revision)
	require.Equal(t, 1, control.Version)
	store := &model.ChannelAdmissionStore{DB: db, LogDB: db}
	from := model.ChannelIdentity{ChannelID: 1, KeyDigest: strings.Repeat("a", 64), IdentityHash: strings.Repeat("b", 64)}
	to := from
	to.ChannelID = 2
	_, err = store.Seal(context.Background(), model.ChannelSealRequest{OperationID: "x", Epoch: "not-activated", From: from, To: to})
	require.ErrorIs(t, err, model.ErrChannelAdmissionUnavailable)
}

func TestAdmissionCorruptDurableResultsFailClosed(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := context.Background()
	request := f.seal("seal", 0, 1, 2, "")
	_, err := f.store.Seal(ctx, request)
	require.NoError(t, err)
	require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionBarrier{}).Where("operation_id = ?", "seal").Update("to_channel_id", 99).Error)
	_, err = f.store.Seal(ctx, request)
	require.ErrorIs(t, err, model.ErrChannelAdmissionConflict)
	input := admissionRequest("use", f.identities[1])
	use, _, err := f.store.Admit(ctx, input, nil)
	require.NoError(t, err)
	require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionUse{}).Where("id = ?", use.ID).Update("state", "closed").Error)
	_, _, err = f.store.Admit(ctx, input, nil)
	require.ErrorIs(t, err, model.ErrChannelAdmissionConflict)
}

func TestAdmissionChangedRetiredPhysicalIDCannotEscapeDigestFence(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := context.Background()
	_, err := f.store.Seal(ctx, f.seal("seal", 0, 1, 2, ""))
	require.NoError(t, err)
	require.NoError(t, f.store.DB.Model(&model.Channel{}).Where("id = ?", 1).Update("key", "synthetic-out-of-band-key").Error)
	var changed model.Channel
	require.NoError(t, f.store.DB.First(&changed, 1).Error)
	identity, err := model.ChannelIdentityOf(&changed)
	require.NoError(t, err)
	_, _, err = f.store.Admit(ctx, admissionRequest("changed-retired", identity), nil)
	require.ErrorIs(t, err, model.ErrChannelAdmissionConflict)
}

func (f *admissionFixture) refreshSyntheticProof(t *testing.T) {
	t.Helper()
	hash, err := f.proof.Fingerprint()
	require.NoError(t, err)
	require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionProof{}).Where("id = ?", f.proof.ProofID).Update("fact_hash", hash).Error)
}

func TestAdmissionFinalCASRejectsProofExpiringDuringJournalWrite(t *testing.T) {
	f := newAdmissionFixture(t)
	f.proof.ValidUntilMs = time.Now().Add(300 * time.Millisecond).UnixMilli()
	f.refreshSyntheticProof(t)
	reached := false
	require.NoError(t, f.store.DB.Callback().Create().Before("gorm:create").Register("test_pause_barrier", func(tx *gorm.DB) {
		if tx.Statement.Table == "channel_admission_barriers" {
			reached = true
			time.Sleep(400 * time.Millisecond)
		}
	}))
	defer f.store.DB.Callback().Create().Remove("test_pause_barrier")
	_, err := f.store.Seal(context.Background(), f.seal("expired", 0, 1, 2, ""))
	require.True(t, reached)
	require.ErrorIs(t, err, model.ErrChannelAdmissionUnavailable)
	var count int64
	require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionBarrier{}).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionFamily{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestAdmissionLoadsProofSnapshotWithoutHoldingGlobalLock(t *testing.T) {
	f := newAdmissionFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	f.store.LoadDeployment = func(context.Context) (model.VerifiedDeploymentProof, error) {
		close(entered)
		<-release
		return f.proof, nil
	}
	done := make(chan error, 1)
	go func() { _, err := f.store.Seal(context.Background(), f.seal("seal", 0, 1, 2, "")); done <- err }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	// This would block/fail if snapshot retrieval held the control write lock.
	err := f.store.DB.WithContext(ctx).Model(&model.ChannelAdmissionControl{}).Where("id = ?", 1).UpdateColumn("revision", gorm.Expr("revision")).Error
	close(release)
	require.NoError(t, err)
	require.NoError(t, <-done)
}

func TestAdmissionRejectsUnfencedExecutorsAndChangedMembership(t *testing.T) {
	for _, kind := range []string{"old-build", "paused-dispatcher", "old-connection-epoch", "unknown-instance", "changed-membership", "revoked", "stale-revocation"} {
		t.Run(kind, func(t *testing.T) {
			f := newAdmissionFixture(t)
			switch kind {
			case "old-build":
				f.store.Executor.BuildSHA = strings.Repeat("a", 64)
			case "paused-dispatcher":
				f.store.Executor.ServiceID = "legacy-dispatcher"
			case "old-connection-epoch":
				f.store.Executor.Epoch = "previous-epoch"
			case "unknown-instance":
				f.store.Executor.InstanceID = "new-member-not-admitted"
			case "changed-membership":
				f.proof.MembershipHash = strings.Repeat("e", 64)
				f.refreshSyntheticProof(t)
			case "revoked":
				require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionProof{}).Where("id = ?", f.proof.ProofID).Update("revoked", true).Error)
			case "stale-revocation":
				f.proof.RevocationFreshUntilMs = time.Now().Add(-time.Second).UnixMilli()
				f.refreshSyntheticProof(t)
			}
			_, _, err := f.store.Admit(context.Background(), admissionRequest("new", f.identities[0]), nil)
			require.ErrorIs(t, err, model.ErrChannelAdmissionUnavailable)
			var count int64
			require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionUse{}).Count(&count).Error)
			require.Zero(t, count)
		})
	}
}

func TestAdmissionRestoredSnapshotBelowIndependentWatermarkRefusesNewWork(t *testing.T) {
	f := newAdmissionFixture(t)
	backup := filepath.Join(t.TempDir(), "before-seal.db")
	require.NoError(t, f.store.DB.Exec("VACUUM INTO ?", backup).Error)
	_, err := f.store.Seal(context.Background(), f.seal("later-seal", 0, 1, 2, ""))
	require.NoError(t, err)
	restored, err := model.OpenSQLite(backup)
	require.NoError(t, err)
	pool, err := restored.DB()
	require.NoError(t, err)
	defer pool.Close()
	proof := f.proof
	proof.StorageWatermark = 1
	hash, err := proof.Fingerprint()
	require.NoError(t, err)
	require.NoError(t, restored.Model(&model.ChannelAdmissionProof{}).Where("id = ?", proof.ProofID).Update("fact_hash", hash).Error)
	store := &model.ChannelAdmissionStore{DB: restored, LogDB: restored, Executor: f.store.Executor, LoadDeployment: func(context.Context) (model.VerifiedDeploymentProof, error) { return proof, nil }}
	_, _, err = store.Admit(context.Background(), admissionRequest("after-restore", f.identities[0]), nil)
	require.ErrorIs(t, err, model.ErrChannelAdmissionUnavailable)
	_, err = store.Seal(context.Background(), f.seal("new-seal-on-old-db", 0, 1, 2, ""))
	require.ErrorIs(t, err, model.ErrChannelAdmissionUnavailable)
	var count int64
	require.NoError(t, restored.Model(&model.ChannelAdmissionBarrier{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestAdmissionActivationConsumesProofOnceAndRejectsReplayAcrossMembers(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := context.Background()
	require.NoError(t, f.store.DB.Where("id = ?", f.proof.ProofID).Delete(&model.ChannelAdmissionProof{}).Error)
	require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionControl{}).Where("id = ?", 1).Updates(map[string]any{"epoch": "", "epoch_sequence": 0, "storage_generation": "", "membership_hash": "", "executor_fence_hash": "", "active_proof_id": ""}).Error)
	first, err := f.store.Activate(ctx, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), first.EpochSequence)
	again, err := f.store.Activate(ctx, 0)
	require.NoError(t, err)
	require.Equal(t, first, again)
	var proofCount int64
	require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionProof{}).Count(&proofCount).Error)
	require.Equal(t, int64(1), proofCount)
	oldProof := f.proof
	f.proof.ProofID = "synthetic-proof-next"
	f.proof.Sequence = 2
	next, err := f.store.Activate(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, int64(2), next.EpochSequence)
	f.proof = oldProof
	_, err = f.store.Activate(ctx, 0)
	require.ErrorIs(t, err, model.ErrChannelAdmissionConflict)
	var current model.ChannelAdmissionControl
	require.NoError(t, f.store.DB.First(&current, 1).Error)
	require.Equal(t, int64(2), current.EpochSequence)
}

func TestAdmissionActivationRejectsUncertainStorageGeneration(t *testing.T) {
	f := newAdmissionFixture(t)
	f.proof.ProofID = "after-restore"
	f.proof.Sequence = 2
	f.proof.StorageGeneration = "independent-new-generation"
	_, err := f.store.Activate(context.Background(), 1)
	require.ErrorIs(t, err, model.ErrChannelAdmissionConflict)
	var proofCount int64
	require.NoError(t, f.store.DB.Model(&model.ChannelAdmissionProof{}).Where("id = ?", f.proof.ProofID).Count(&proofCount).Error)
	require.Zero(t, proofCount)
}
