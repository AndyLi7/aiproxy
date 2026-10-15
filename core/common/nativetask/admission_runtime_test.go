package nativetask

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func activateNativeRuntime(t *testing.T, e *Engine, p *Plan) (*model.ChannelAdmissionStore, []model.ChannelIdentity) {
	t.Helper()
	db := e.DB
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.ChannelAdmissionControl{}, &model.ChannelAdmissionProof{}, &model.ChannelAdmissionFamily{}, &model.ChannelAdmissionBarrier{}, &model.ChannelAdmissionUse{}))
	require.NoError(t, db.Create(&model.ChannelAdmissionControl{ID: 1, Version: 1}).Error)
	identities := []model.ChannelIdentity{}
	for i := 1; i <= 2; i++ {
		c := model.Channel{ID: i, Key: "test-native-key", Type: model.ChannelTypeFal, Status: model.ChannelStatusEnabled, BaseURL: "https://example.invalid/" + strings.Repeat("x", i)}
		require.NoError(t, db.Create(&c).Error)
		identity, err := model.ChannelIdentityOf(&c)
		require.NoError(t, err)
		identities = append(identities, identity)
	}
	p.AdmissionIdentityHash = identities[0].IdentityHash
	executor := model.ChannelExecutorIdentity{ServiceID: "synthetic", InstanceID: "native-test", BuildSHA: strings.Repeat("a", 64), Epoch: "synthetic"}
	proof := model.VerifiedDeploymentProof{ProofID: "synthetic", Epoch: "synthetic", Sequence: 1, StorageGeneration: "synthetic", ExecutorFenceHash: strings.Repeat("b", 64), Executors: []model.ChannelExecutorIdentity{executor}, IssuedAtMs: time.Now().Add(-time.Minute).UnixMilli(), ValidUntilMs: time.Now().Add(time.Minute).UnixMilli(), RevocationFreshUntilMs: time.Now().Add(time.Minute).UnixMilli()}
	raw, err := json.Marshal(proof.Executors)
	require.NoError(t, err)
	digest := sha256.Sum256(raw)
	proof.MembershipHash = hex.EncodeToString(digest[:])
	store := &model.ChannelAdmissionStore{DB: db, LogDB: db, Executor: executor, LoadDeployment: func(context.Context) (model.VerifiedDeploymentProof, error) { return proof, nil }}
	_, err = store.Activate(context.Background(), 0)
	require.NoError(t, err)
	oldDB, oldLog := model.DB, model.LogDB
	model.DB, model.LogDB = db, db
	restore := model.ConfigureChannelAdmissionRuntime(executor, store.LoadDeployment)
	t.Cleanup(func() { restore(); model.DB, model.LogDB = oldDB, oldLog })
	return store, identities
}

type admissionObservingWallet struct {
	t     *testing.T
	e     *Engine
	fail  error
	calls int
}

func (w *admissionObservingWallet) Prepayment(_ context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	w.calls++
	var uses []model.ChannelAdmissionUse
	require.NoError(w.t, w.e.DB.Find(&uses).Error)
	require.NotEmpty(w.t, uses)
	var task model.NativeTask
	require.NoError(w.t, w.e.DB.First(&task, "billing_operation_id = ?", c.BillingOperationID).Error)
	if c.Action == "admit" {
		return balance.PrepaymentReceipt{}, w.fail
	}
	return balance.PrepaymentReceipt{}, errors.New("unexpected wallet action")
}
func TestAdmissionNativeEngineRejectsSealedPublicAndDirectBeforeWallet(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(map[bool]string{false: "public", true: "trial-direct"}[direct], func(t *testing.T) {
			e, p, w, provider := setup(t)
			store, ids := activateNativeRuntime(t, e, &p)
			p.Direct = direct
			_, err := store.Seal(context.Background(), model.ChannelSealRequest{OperationID: "move", Epoch: "synthetic", From: ids[0], To: ids[1]})
			require.NoError(t, err)
			_, err = e.Submit(context.Background(), "sealed", "g", 1, body, p)
			require.ErrorIs(t, err, model.ErrChannelAdmissionSealed)
			require.Empty(t, w.commands)
			require.Zero(t, provider.calls)
		})
	}
}
func TestAdmissionNativeEngineWalletRefusalClosesOnlyCurrentGeneration(t *testing.T) {
	e, p, _, provider := setup(t)
	activateNativeRuntime(t, e, &p)
	wallet := &admissionObservingWallet{t: t, e: e, fail: balance.ErrPrepaymentInsufficientBalance}
	e.Wallet = wallet
	for range 2 {
		_, err := e.Submit(context.Background(), "retry", "g", 1, body, p)
		require.True(t, WalletRefusal(err))
	}
	require.Equal(t, 2, wallet.calls)
	require.Zero(t, provider.calls)
	var uses []model.ChannelAdmissionUse
	require.NoError(t, e.DB.Find(&uses).Error)
	require.Len(t, uses, 2)
	for _, use := range uses {
		require.Equal(t, "closed", use.State)
	}
	require.NotEqual(t, uses[0].ID, uses[1].ID)
	var count int64
	require.NoError(t, e.DB.Model(&model.NativeTask{}).Count(&count).Error)
	require.Zero(t, count)
}
func TestAdmissionNativeEngineUnknownWalletRetainsTaskAndUse(t *testing.T) {
	e, p, _, provider := setup(t)
	activateNativeRuntime(t, e, &p)
	wallet := &admissionObservingWallet{t: t, e: e, fail: errors.New("unknown wallet outcome")}
	e.Wallet = wallet
	for range 2 {
		_, err := e.Submit(context.Background(), "unknown", "g", 1, body, p)
		require.Error(t, err)
	}
	require.Zero(t, provider.calls)
	var uses []model.ChannelAdmissionUse
	require.NoError(t, e.DB.Find(&uses).Error)
	require.Len(t, uses, 1)
	require.Equal(t, "open", uses[0].State)
	var count int64
	require.NoError(t, e.DB.Model(&model.NativeTask{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
}

type lateGenerationWallet struct {
	action  string
	replace func()
	waiting bool
}

func (w *lateGenerationWallet) Prepayment(_ context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	if c.Action == w.action {
		w.replace()
		w.replace = func() {}
	}
	if c.Action == "admit" && w.waiting {
		return balance.PrepaymentReceipt{Status: "pending", SubmissionStatus: json.RawMessage(`"waiting"`)}, nil
	}
	return balance.PrepaymentReceipt{Claimed: true, Status: "pending"}, nil
}
func TestAdmissionNativeLatePositiveWalletCannotAdvanceNewGeneration(t *testing.T) {
	for _, kind := range []string{"begin-attempt", "waiting", "billing-get"} {
		t.Run(kind, func(t *testing.T) {
			e, p, _, provider := setup(t)
			activateNativeRuntime(t, e, &p)
			replace := func() {
				current, err := model.GetNativeTask(e.DB, "late", "g", 1)
				require.NoError(t, err)
				evidence, err := model.CaptureNativeReservationRelease(e.DB, current, "")
				require.NoError(t, err)
				released, err := model.ReleaseNativeReservation(e.DB, current.ID, current.GroupID, current.TokenID, current.UpdatedAt, evidence)
				require.NoError(t, err)
				require.True(t, released)
				current.AdmissionIdentityHash = p.AdmissionIdentityHash
				current.CreatedAt = time.Time{}
				current.UpdatedAt = time.Time{}
				_, created, err := model.ReserveNativeTask(e.DB, *current)
				require.NoError(t, err)
				require.True(t, created)
			}
			wallet := &lateGenerationWallet{action: "begin_attempt", replace: replace}
			if kind == "waiting" {
				wallet.action = "admit"
				wallet.waiting = true
			}
			if kind == "billing-get" {
				e.Wallet = &scriptedWallet{errs: map[string]error{"admit": errors.New("unknown")}}
				_, err := e.Submit(context.Background(), "late", "g", 1, body, p)
				require.Error(t, err)
				first, err := model.GetNativeTask(e.DB, "late", "g", 1)
				require.NoError(t, err)
				wallet.action = "get"
				e.Wallet = wallet
				require.ErrorIs(t, e.SyncBilling(context.Background(), first), model.ErrNativeTaskConflict)
			} else {
				e.Wallet = wallet
				_, err := e.Submit(context.Background(), "late", "g", 1, body, p)
				require.ErrorIs(t, err, model.ErrNativeTaskConflict)
			}
			fresh, err := model.GetNativeTask(e.DB, "late", "g", 1)
			require.NoError(t, err)
			require.Equal(t, "reserved", fresh.Status)
			require.Empty(t, fresh.BillingReceiptJSON)
			require.Zero(t, provider.calls)
			var uses []model.ChannelAdmissionUse
			require.NoError(t, e.DB.Find(&uses).Error)
			require.Len(t, uses, 2)
		})
	}
}
func TestAdmissionNativeRecoveryRequiresDefiniteWalletAbsence(t *testing.T) {
	for _, kind := range []string{"not-found", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			e, p, _, provider := setup(t)
			activateNativeRuntime(t, e, &p)
			e.Wallet = &scriptedWallet{errs: map[string]error{"admit": errors.New("unknown")}}
			_, err := e.Submit(context.Background(), "stale", "g", 1, body, p)
			require.Error(t, err)
			past := time.Now().Add(-2 * time.Hour)
			require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "stale").Updates(map[string]any{"created_at": past, "updated_at": past}).Error)
			failure := errors.New("wallet unavailable")
			if kind == "not-found" {
				failure = walletNotFound(t, appNotFound)
			}
			e.Wallet = &scriptedWallet{errs: map[string]error{"get": failure}}
			report, err := e.RecoverOnce(context.Background(), "recovery-worker", time.Now(), nil, nil)
			require.NoError(t, err)
			require.Equal(t, 1, report.Claimed)
			require.Zero(t, provider.calls)
			var uses []model.ChannelAdmissionUse
			require.NoError(t, e.DB.Find(&uses).Error)
			require.Len(t, uses, 1)
			if kind == "not-found" {
				require.Equal(t, "closed", uses[0].State)
				require.Equal(t, 1, report.Advanced)
			} else {
				require.Equal(t, "open", uses[0].State)
				require.Equal(t, 1, report.Deferred)
			}
		})
	}
}

func TestAdmissionNativeRecoveryLateReceiptRequiresLiveClaim(t *testing.T) {
	for _, kind := range []string{"same-owner", "owner-lost", "lease-expired", "new-generation"} {
		t.Run(kind, func(t *testing.T) {
			e, p, _, provider := setup(t)
			activateNativeRuntime(t, e, &p)
			e.Wallet = &scriptedWallet{errs: map[string]error{"admit": errors.New("unknown")}}
			_, err := e.Submit(context.Background(), "recover-receipt", "g", 1, body, p)
			require.Error(t, err)
			e.Wallet = &lateGenerationWallet{action: "get", replace: func() {
				if kind == "new-generation" {
					current, readErr := model.GetNativeTask(e.DB, "recover-receipt", "g", 1)
					require.NoError(t, readErr)
					evidence, captureErr := model.CaptureNativeReservationRelease(e.DB, current, "worker")
					require.NoError(t, captureErr)
					released, releaseErr := model.ReleaseNativeReservation(e.DB, current.ID, current.GroupID, current.TokenID, time.Now().Add(time.Hour), evidence)
					require.NoError(t, releaseErr)
					require.True(t, released)
					current.AdmissionIdentityHash = p.AdmissionIdentityHash
					current.CreatedAt = time.Time{}
					current.UpdatedAt = time.Time{}
					_, created, reserveErr := model.ReserveNativeTask(e.DB, *current)
					require.NoError(t, reserveErr)
					require.True(t, created)
				}
				if kind == "owner-lost" {
					require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "recover-receipt").UpdateColumn("recovery_owner", "another-worker").Error)
				}
				if kind == "lease-expired" {
					require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "recover-receipt").UpdateColumn("recovery_until", time.Now().Add(-time.Second).Unix()).Error)
				}
			}}
			_, err = e.RecoverOnce(context.Background(), "worker", time.Now(), nil, nil)
			if kind == "owner-lost" || kind == "new-generation" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			task, err := model.GetNativeTask(e.DB, "recover-receipt", "g", 1)
			require.NoError(t, err)
			require.Equal(t, "reserved", task.Status)
			require.Zero(t, provider.calls)
			if kind == "same-owner" {
				require.NotEmpty(t, task.BillingReceiptJSON)
			} else {
				require.Empty(t, task.BillingReceiptJSON)
			}
		})
	}
}

func TestAdmissionNativeSameGenerationSuccessAndFrozenReplay(t *testing.T) {
	e, p, _, provider := setup(t)
	store, ids := activateNativeRuntime(t, e, &p)
	task, err := e.Submit(context.Background(), "success", "g", 1, body, p)
	require.NoError(t, err)
	require.Equal(t, "queued", task.Status)
	require.Equal(t, 1, provider.calls)
	_, err = store.Seal(context.Background(), model.ChannelSealRequest{OperationID: "move", Epoch: "synthetic", From: ids[0], To: ids[1]})
	require.NoError(t, err)
	restore := model.ConfigureChannelAdmissionRuntime(store.Executor, nil)
	defer restore()
	_, err = e.Submit(context.Background(), "success", "g", 1, body, p)
	require.NoError(t, err)
	require.Equal(t, 1, provider.calls)
	var uses []model.ChannelAdmissionUse
	require.NoError(t, e.DB.Find(&uses).Error)
	require.Len(t, uses, 1)
	require.Equal(t, "open", uses[0].State)
}
