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
	"gorm.io/gorm"
	"sync"
	"testing"
	"time"
)

func installWorkerHandoff(t *testing.T, e *Engine) {
	t.Helper()
	require.NoError(t, e.DB.AutoMigrate(&model.ChannelAdmissionDispatchHandoff{}))
	require.NoError(t, e.DB.Create(&model.ChannelAdmissionExecutionProtocol{ID: 3, Version: 1}).Error)
}
func dispatchHandoffs(t *testing.T, e *Engine) []model.ChannelAdmissionDispatchHandoff {
	t.Helper()
	var rows []model.ChannelAdmissionDispatchHandoff
	require.NoError(t, e.DB.Find(&rows).Error)
	return rows
}
func setupDispatchHandoffQueue(t *testing.T, installed bool) (*queueEnv, *model.ChannelAdmissionStore) {
	env := newQueueEnv(t)
	store, _ := activateNativeRuntime(t, env.e, &env.plan)
	installNativeInvocationJournal(t, env.e)
	_, err := env.submit("holder", "g0")
	require.NoError(t, err)
	_, err = env.submit("handoff", "g1")
	require.NoError(t, err)
	require.Equal(t, model.NativeStatusWaiting, env.task("handoff", "g1").Status)
	env.finish("holder")
	if installed {
		installWorkerHandoff(t, env.e)
	}
	return env, store
}

type cleanupDispatchProvider struct {
	base             Provider
	entered, release chan struct{}
}

func (p *cleanupDispatchProvider) SubmitNative(ctx context.Context, endpoint string, body []byte) (string, error) {
	defer func() { close(p.entered); <-p.release }()
	return p.base.SubmitNative(ctx, endpoint, body)
}
func TestDispatchHandoffActualRoundWaitsForOwnedCleanup(t *testing.T) {
	env, _ := setupDispatchHandoffQueue(t, true)
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	provider := &cleanupDispatchProvider{base: env.p, entered: entered, release: release}
	resolver := env.e.QueueProviders["fal"]
	resolver.Resolve = func(context.Context, *model.NativeTask) (Provider, error) { return provider, nil }
	env.e.QueueProviders["fal"] = resolver
	env.w.beforeBegin = func(c balance.PrepaymentCommand) {
		if c.BillingOperationID == "native:handoff" {
			rows := dispatchHandoffs(t, env.e)
			require.Len(t, rows, 1)
			require.Equal(t, "accepted", rows[0].State)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var running sync.WaitGroup
	env.e.DispatchRound(ctx, &running)
	done := make(chan struct{})
	go func() { running.Wait(); close(done) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider cleanup was not reached")
	}
	cancel()
	row := dispatchHandoffs(t, env.e)[0]
	require.Equal(t, "accepted", row.State)
	require.Zero(t, row.ExitedAtMs)
	select {
	case <-done:
		t.Fatal("supervisor returned before worker cleanup")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not finish")
	}
	row = dispatchHandoffs(t, env.e)[0]
	require.Equal(t, "exited", row.State)
	var execution model.ChannelAdmissionTaskExecution
	require.NoError(t, env.e.DB.First(&execution, "id = ?", row.ID).Error)
	require.Equal(t, "returned", execution.State)
	require.Equal(t, "unproven", execution.ContinuationState)
	require.Equal(t, 2, env.p.count())
	require.Equal(t, 1, countWalletAction(env.w, "native:handoff", "begin_attempt"))
	var uses []model.ChannelAdmissionUse
	require.NoError(t, env.e.DB.Find(&uses).Error)
	for _, use := range uses {
		require.Equal(t, "open", use.State)
	}
}
func countWalletAction(w *queueWallet, id, action string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, c := range w.commands {
		if c.BillingOperationID == id && c.Action == action {
			n++
		}
	}
	return n
}
func TestDispatchHandoffActualPrepareFailureNeverForksOrResetsAge(t *testing.T) {
	for _, mode := range []string{"partial", "wrong-version", "offer-write"} {
		t.Run(mode, func(t *testing.T) {
			env, _ := setupDispatchHandoffQueue(t, true)
			before := env.task("handoff", "g1")
			enqueued := *before.EnqueuedAt
			switch mode {
			case "partial":
				require.NoError(t, env.e.DB.Where("id = ?", 3).Delete(&model.ChannelAdmissionExecutionProtocol{}).Error)
			case "wrong-version":
				require.NoError(t, env.e.DB.Model(&model.ChannelAdmissionExecutionProtocol{}).Where("id = ?", 3).Update("version", 99).Error)
			case "offer-write":
				require.NoError(t, env.e.DB.Callback().Create().After("gorm:create").Register("test:worker-offer-fail", func(tx *gorm.DB) {
					if tx.Statement.Table == "channel_admission_dispatch_handoffs" {
						tx.AddError(errors.New("offer unavailable"))
					}
				}))
				defer env.e.DB.Callback().Create().Remove("test:worker-offer-fail")
			}
			env.round()
			after := env.task("handoff", "g1")
			require.Equal(t, model.NativeStatusDispatching, after.Status)
			require.Equal(t, enqueued, *after.EnqueuedAt)
			require.Equal(t, before.CreatedAt, after.CreatedAt)
			require.Equal(t, 1, env.p.count())
			require.Zero(t, countWalletAction(env.w, "native:handoff", "begin_attempt"))
			require.Empty(t, dispatchHandoffs(t, env.e))
		})
	}
}
func TestDispatchHandoffCompatibilityAndMidFlightInstall(t *testing.T) {
	for _, mode := range []string{"absent", "install-during-wallet", "source-unavailable", "epoch-switch"} {
		t.Run(mode, func(t *testing.T) {
			installed := mode == "source-unavailable" || mode == "epoch-switch"
			env, store := setupDispatchHandoffQueue(t, installed)
			if mode == "install-during-wallet" {
				env.w.beforeBegin = func(c balance.PrepaymentCommand) {
					if c.BillingOperationID == "native:handoff" {
						installWorkerHandoff(t, env.e)
					}
				}
			}
			if mode == "source-unavailable" {
				restore := model.ConfigureChannelAdmissionRuntime(store.Executor, nil)
				defer restore()
			}
			if mode == "epoch-switch" {
				proof, err := store.LoadDeployment(t.Context())
				require.NoError(t, err)
				proof.ProofID = "dispatch-epoch-2"
				proof.Epoch = "dispatch-epoch-2"
				proof.Sequence = 2
				executor := store.Executor
				executor.Epoch = proof.Epoch
				executor.InstanceID = "next-dispatcher"
				proof.Executors = []model.ChannelExecutorIdentity{executor}
				raw, err := json.Marshal(proof.Executors)
				require.NoError(t, err)
				sum := sha256.Sum256(raw)
				proof.MembershipHash = hex.EncodeToString(sum[:])
				store.Executor = executor
				store.LoadDeployment = func(context.Context) (model.VerifiedDeploymentProof, error) { return proof, nil }
				_, err = store.Activate(t.Context(), 1)
				require.NoError(t, err)
				restore := model.ConfigureChannelAdmissionRuntime(executor, store.LoadDeployment)
				defer restore()
			}
			env.round()
			require.Equal(t, "queued", env.task("handoff", "g1").Status)
			require.Equal(t, 2, env.p.count())
			require.Equal(t, 1, countWalletAction(env.w, "native:handoff", "begin_attempt"))
			if mode != "absent" {
				rows := dispatchHandoffs(t, env.e)
				if mode == "epoch-switch" || mode == "source-unavailable" {
					require.Len(t, rows, 1)
					require.Equal(t, "exited", rows[0].State)
				} else {
					require.Empty(t, rows, "uncovered old fork must never acquire retrospective handoff")
				}
			}
		})
	}
}

type panicDispatchProvider struct{}

func (panicDispatchProvider) SubmitNative(context.Context, string, []byte) (string, error) {
	panic("synthetic owned worker panic")
}
func TestDispatchHandoffPanicAndExitWriteFailureRetain(t *testing.T) {
	for _, mode := range []string{"panic", "exit-write"} {
		t.Run(mode, func(t *testing.T) {
			env, _ := setupDispatchHandoffQueue(t, true)
			if mode == "panic" {
				task, _, err := env.e.claim(falScope, "")
				require.NoError(t, err)
				require.NotNil(t, task)
				h, err := model.PrepareNativeDispatchHandoff(t.Context(), env.e.DB, task)
				require.NoError(t, err)
				resolver := env.e.QueueProviders["fal"]
				resolver.Resolve = func(context.Context, *model.NativeTask) (Provider, error) { return panicDispatchProvider{}, nil }
				env.e.QueueProviders["fal"] = resolver
				done := make(chan dispatchObservation, 1)
				require.Panics(t, func() {
					observeDispatchWorker(func() dispatchObservation { return env.e.dispatchHandoffWorker(t.Context(), task, h) }, done)
				})
				result := <-done
				require.False(t, result.returned)
				recordDispatchObservation(task, h, result)
			} else {
				require.NoError(t, env.e.DB.Callback().Update().After("gorm:update").Register("test:exit-fail", func(tx *gorm.DB) {
					changes, ok := tx.Statement.Dest.(map[string]any)
					if ok && tx.Statement.Table == "channel_admission_dispatch_handoffs" && changes["state"] == "exited" {
						tx.AddError(errors.New("exit write failed"))
					}
				}))
				defer env.e.DB.Callback().Update().Remove("test:exit-fail")
				env.round()
			}
			rows := dispatchHandoffs(t, env.e)
			require.Len(t, rows, 1)
			require.Equal(t, "accepted", rows[0].State)
			require.Zero(t, rows[0].ExitedAtMs)
			var execution model.ChannelAdmissionTaskExecution
			require.NoError(t, env.e.DB.First(&execution, "id = ?", rows[0].ID).Error)
			require.Equal(t, "active", execution.State)
			require.Equal(t, "unproven", execution.ContinuationState)
		})
	}
}

type continuationDispatchProvider struct {
	base                       Provider
	started, release, finished chan struct{}
}

func (p *continuationDispatchProvider) SubmitNative(ctx context.Context, endpoint string, body []byte) (string, error) {
	go func() { close(p.started); <-p.release; close(p.finished) }()
	return p.base.SubmitNative(ctx, endpoint, body)
}
func TestDispatchHandoffExitDoesNotCertifyUnjoinedProviderWork(t *testing.T) {
	env, _ := setupDispatchHandoffQueue(t, true)
	provider := &continuationDispatchProvider{base: env.p, started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	defer func() { close(provider.release); <-provider.finished }()
	resolver := env.e.QueueProviders["fal"]
	resolver.Resolve = func(context.Context, *model.NativeTask) (Provider, error) { return provider, nil }
	env.e.QueueProviders["fal"] = resolver
	env.round()
	<-provider.started
	require.Equal(t, "exited", dispatchHandoffs(t, env.e)[0].State)
	select {
	case <-provider.finished:
		t.Fatal("fixture continuation unexpectedly finished")
	default:
	}
	var execution model.ChannelAdmissionTaskExecution
	require.NoError(t, env.e.DB.First(&execution, "id = ?", dispatchHandoffs(t, env.e)[0].ID).Error)
	require.Equal(t, "unproven", execution.ContinuationState)
	var use model.ChannelAdmissionUse
	require.NoError(t, env.e.DB.First(&use, "id = ?", execution.UseID).Error)
	require.Equal(t, "open", use.State)
}

func TestDispatchHandoffLateExtensionsDoNotInventCoverage(t *testing.T) {
	env := newQueueEnv(t)
	store, _ := activateNativeRuntime(t, env.e, &env.plan)
	_, err := env.submit("holder", "g0")
	require.NoError(t, err)
	_, err = env.submit("handoff", "g1")
	require.NoError(t, err)
	env.finish("holder")
	installNativeInvocationJournal(t, env.e)
	installWorkerHandoff(t, env.e)
	restore := model.ConfigureChannelAdmissionRuntime(store.Executor, nil)
	defer restore()
	env.round()
	require.Equal(t, "queued", env.task("handoff", "g1").Status)
	require.Equal(t, 2, env.p.count())
	require.Empty(t, dispatchHandoffs(t, env.e))
	require.Empty(t, nativeInvocations(t, env.e))
	var uses []model.ChannelAdmissionUse
	require.NoError(t, env.e.DB.Find(&uses).Error)
	for _, use := range uses {
		require.Equal(t, "open", use.State)
	}
}
func TestDispatchHandoffActualChildCannotRunTwiceOrAfterLeaseLoss(t *testing.T) {
	for _, mode := range []string{"duplicate", "expired", "owner"} {
		t.Run(mode, func(t *testing.T) {
			env, _ := setupDispatchHandoffQueue(t, true)
			task, _, err := env.e.claim(falScope, "")
			require.NoError(t, err)
			h, err := model.PrepareNativeDispatchHandoff(t.Context(), env.e.DB, task)
			require.NoError(t, err)
			if mode == "expired" {
				require.NoError(t, env.e.DB.Model(task).Update("dispatch_until", time.Now().Add(-time.Second).UTC()).Error)
			}
			if mode == "owner" {
				require.NoError(t, env.e.DB.Model(task).Update("dispatch_owner", "other-child-claim").Error)
			}
			done := make(chan dispatchObservation, 1)
			observeDispatchWorker(func() dispatchObservation { return env.e.dispatchHandoffWorker(t.Context(), task, h) }, done)
			first := <-done
			if mode == "duplicate" {
				require.True(t, first.accepted)
				observeDispatchWorker(func() dispatchObservation { return env.e.dispatchHandoffWorker(t.Context(), task, h) }, done)
				second := <-done
				require.False(t, second.accepted)
				recordDispatchObservation(task, h, second)
				require.Equal(t, "accepted", dispatchHandoffs(t, env.e)[0].State)
				recordDispatchObservation(task, h, first)
				require.Equal(t, "exited", dispatchHandoffs(t, env.e)[0].State)
				require.Equal(t, 2, env.p.count())
				require.Equal(t, 1, countWalletAction(env.w, "native:handoff", "begin_attempt"))
			} else {
				require.False(t, first.accepted)
				recordDispatchObservation(task, h, first)
				require.Equal(t, "offered", dispatchHandoffs(t, env.e)[0].State)
				require.Equal(t, 1, env.p.count())
				require.Zero(t, countWalletAction(env.w, "native:handoff", "begin_attempt"))
			}
		})
	}
}
