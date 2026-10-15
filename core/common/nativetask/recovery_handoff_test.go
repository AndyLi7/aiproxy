package nativetask

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"runtime"
	"sync"
	"testing"
	"time"
)

func installRecoveryWorkerHandoff(t *testing.T, e *Engine) {
	t.Helper()
	require.NoError(t, e.DB.AutoMigrate(&model.ChannelAdmissionRecoveryHandoff{}))
	require.NoError(t, e.DB.Create(&model.ChannelAdmissionExecutionProtocol{ID: 4, Version: 1}).Error)
}
func workerRecoveryHandoffs(t *testing.T, e *Engine) []model.ChannelAdmissionRecoveryHandoff {
	t.Helper()
	var rows []model.ChannelAdmissionRecoveryHandoff
	require.NoError(t, e.DB.Find(&rows).Error)
	return rows
}
func setupRecoveryHandoff(t *testing.T, installed bool) (*Engine, *model.ChannelAdmissionStore, *walletStub) {
	t.Helper()
	e, p, w, _ := setup(t)
	store, _ := activateNativeRuntime(t, e, &p)
	installNativeInvocationJournal(t, e)
	_, err := e.Submit(t.Context(), "recovery", "g", 1, body, p)
	require.NoError(t, err)
	if installed {
		installRecoveryWorkerHandoff(t, e)
	}
	return e, store, w
}

type recoveryPollFunc func(context.Context) (nativeresult.PollResult, error)

func (f recoveryPollFunc) PollNative(ctx context.Context, _ string, _ string, _ *nativeresult.CompiledTask) (nativeresult.PollResult, error) {
	return f(ctx)
}

type recoveryWalletFunc func(context.Context, balance.PrepaymentCommand) (balance.PrepaymentReceipt, error)

func (f recoveryWalletFunc) Prepayment(ctx context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	return f(ctx, c)
}
func recoveryResolver(p Poller) ResolvePoller {
	return func(context.Context, *model.NativeTask) (Poller, error) { return p, nil }
}
func assertRecoveryOuter(t *testing.T, e *Engine, state string) {
	t.Helper()
	rows := workerRecoveryHandoffs(t, e)
	require.Len(t, rows, 1)
	require.Equal(t, state, rows[0].State)
	var execution model.ChannelAdmissionTaskExecution
	require.NoError(t, e.DB.First(&execution, "id = ?", rows[0].ID).Error)
	require.Equal(t, "unproven", execution.ContinuationState)
	if state == "accepted" {
		require.Equal(t, "active", execution.State)
		require.Zero(t, execution.ReturnedAtMs)
	} else {
		require.Contains(t, []string{"returned", "error_returned"}, execution.State)
		require.Equal(t, rows[0].ExitedAtMs, execution.ReturnedAtMs)
	}
	var uses []model.ChannelAdmissionUse
	require.NoError(t, e.DB.Find(&uses).Error)
	for _, use := range uses {
		require.Equal(t, "open", use.State)
	}
}
func TestRecoveryHandoffActualOwnedCleanup(t *testing.T) {
	for _, phase := range []string{"poll", "billing"} {
		t.Run(phase, func(t *testing.T) {
			e, _, w := setupRecoveryHandoff(t, true)
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var pollCtx context.Context
			var cleanupOnce sync.Once
			p := recoveryPollFunc(func(ctx context.Context) (nativeresult.PollResult, error) {
				pollCtx = ctx
				if phase == "poll" {
					defer cleanupOnce.Do(func() { close(entered); <-release })
					return nativeresult.PollResult{Status: "running"}, nil
				}
				return nativeresult.PollResult{Status: "result_received", Output: []byte(`{"labels":[]}`)}, nil
			})
			if phase == "billing" {
				e.Wallet = recoveryWalletFunc(func(ctx context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
					if c.Action == "execution_finished" {
						defer cleanupOnce.Do(func() { close(entered); <-release })
					}
					return w.Prepayment(ctx, c)
				})
			}
			done := make(chan error, 1)
			go func() {
				_, err := e.RecoverOnce(context.Background(), "worker", time.Now(), recoveryResolver(p), nil)
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("owned cleanup not reached")
			}
			assertRecoveryOuter(t, e, "accepted")
			task, err := model.GetNativeTask(e.DB, "recovery", "g", 1)
			require.NoError(t, err)
			require.NotEmpty(t, task.RecoveryOwner)
			select {
			case <-done:
				t.Fatal("returned before cleanup")
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			select {
			case err = <-done:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("recovery did not return")
			}
			require.ErrorIs(t, pollCtx.Err(), context.Canceled)
			assertRecoveryOuter(t, e, "exited")
			task, err = model.GetNativeTask(e.DB, "recovery", "g", 1)
			require.NoError(t, err)
			require.Empty(t, task.RecoveryOwner)
			require.Zero(t, task.RecoveryUntil)
		})
	}
}
func TestRecoveryHandoffActualAbnormalAndUnjoinedWork(t *testing.T) {
	for _, mode := range []string{"panic", "goexit", "unjoined"} {
		t.Run(mode, func(t *testing.T) {
			e, _, _ := setupRecoveryHandoff(t, true)
			done := make(chan struct{})
			background := make(chan struct{})
			stop := make(chan struct{})
			defer close(stop)
			p := recoveryPollFunc(func(context.Context) (nativeresult.PollResult, error) {
				switch mode {
				case "panic":
					panic("synthetic recovery panic")
				case "goexit":
					runtime.Goexit()
				case "unjoined":
					go func() { defer close(background); <-stop }()
				}
				return nativeresult.PollResult{Status: "running"}, nil
			})
			var err error
			go func() {
				defer close(done)
				defer func() { _ = recover() }()
				_, err = e.RecoverOnce(context.Background(), "worker", time.Now(), recoveryResolver(p), nil)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not stop")
			}
			if mode == "unjoined" {
				require.NoError(t, err)
				assertRecoveryOuter(t, e, "exited")
				select {
				case <-background:
					t.Fatal("background unexpectedly joined")
				default:
				}
			} else {
				assertRecoveryOuter(t, e, "accepted")
				task, e2 := model.GetNativeTask(e.DB, "recovery", "g", 1)
				require.NoError(t, e2)
				require.NotEmpty(t, task.RecoveryOwner)
			}
		})
	}
}
func TestRecoveryHandoffActualCompatibility(t *testing.T) {
	for _, mode := range []string{"absent", "late", "outage", "epoch", "wrong-version", "partial", "offer-fails", "exit-fails"} {
		t.Run(mode, func(t *testing.T) {
			installed := mode != "absent" && mode != "late"
			e, store, w := setupRecoveryHandoff(t, installed)
			if mode == "outage" {
				restore := model.ConfigureChannelAdmissionRuntime(store.Executor, nil)
				defer restore()
			}
			if mode == "epoch" {
				proof, err := store.LoadDeployment(t.Context())
				require.NoError(t, err)
				proof.ProofID = "recovery-next"
				proof.Epoch = "recovery-next"
				proof.Sequence = 2
				executor := store.Executor
				executor.Epoch = proof.Epoch
				executor.InstanceID = "next-recovery"
				proof.Executors = []model.ChannelExecutorIdentity{executor}
				raw, e2 := json.Marshal(proof.Executors)
				require.NoError(t, e2)
				sum := sha256.Sum256(raw)
				proof.MembershipHash = hex.EncodeToString(sum[:])
				next := *store
				next.Executor = executor
				next.LoadDeployment = func(context.Context) (model.VerifiedDeploymentProof, error) { return proof, nil }
				_, err = next.Activate(t.Context(), 1)
				require.NoError(t, err)
				restore := model.ConfigureChannelAdmissionRuntime(executor, next.LoadDeployment)
				defer restore()
			}
			switch mode {
			case "wrong-version":
				require.NoError(t, e.DB.Model(&model.ChannelAdmissionExecutionProtocol{}).Where("id=4").Update("version", 99).Error)
			case "partial":
				require.NoError(t, e.DB.Where("id=4").Delete(&model.ChannelAdmissionExecutionProtocol{}).Error)
			case "offer-fails":
				require.NoError(t, e.DB.Callback().Create().After("gorm:create").Register("test:offer", func(tx *gorm.DB) {
					if tx.Statement.Table == "channel_admission_recovery_handoffs" {
						tx.AddError(errors.New("offer failed"))
					}
				}))
				defer e.DB.Callback().Create().Remove("test:offer")
			}
			before := len(w.commands)
			calls := 0
			p := recoveryPollFunc(func(context.Context) (nativeresult.PollResult, error) {
				calls++
				if mode == "late" {
					installRecoveryWorkerHandoff(t, e)
				}
				if mode == "exit-fails" {
					require.NoError(t, e.DB.Callback().Update().After("gorm:update").Register("test:exit", func(tx *gorm.DB) {
						if tx.Statement.Table == "channel_admission_recovery_handoffs" {
							tx.AddError(errors.New("exit failed"))
						}
					}))
				}
				return nativeresult.PollResult{Status: "running"}, nil
			})
			_, err := e.RecoverOnce(t.Context(), "worker", time.Now(), recoveryResolver(p), nil)
			if mode == "exit-fails" {
				require.NoError(t, e.DB.Callback().Update().Remove("test:exit"))
				require.Error(t, err)
				assertRecoveryOuter(t, e, "accepted")
				task, e2 := model.GetNativeTask(e.DB, "recovery", "g", 1)
				require.NoError(t, e2)
				require.NotEmpty(t, task.RecoveryOwner)
				return
			}
			if mode == "partial" || mode == "wrong-version" || mode == "offer-fails" {
				require.Error(t, err)
				require.Zero(t, calls)
				require.Len(t, w.commands, before)
				require.Empty(t, workerRecoveryHandoffs(t, e))
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, calls)
			if installed {
				assertRecoveryOuter(t, e, "exited")
			} else if mode == "late" {
				require.Empty(t, workerRecoveryHandoffs(t, e))
			}
		})
	}
}
func TestRecoveryHandoffActualClaimCannotBorrowReplacement(t *testing.T) {
	for _, mode := range []string{"same-second-reclaim", "extended"} {
		t.Run(mode, func(t *testing.T) {
			e, _, _ := setupRecoveryHandoff(t, true)
			now := time.Now()
			var nextOwner string
			p := recoveryPollFunc(func(context.Context) (nativeresult.PollResult, error) {
				current, err := model.GetNativeTask(e.DB, "recovery", "g", 1)
				require.NoError(t, err)
				if mode == "extended" {
					require.NoError(t, model.ExtendNativeRecovery(e.DB, current.ID, current.RecoveryOwner, now.Add(11*time.Minute)))
				} else {
					require.NoError(t, model.ReleaseNativeRecovery(e.DB, current.ID, current.RecoveryOwner, now))
					claims, err := model.ClaimNativeRecovery(e.DB, "same-worker", now, 1)
					require.NoError(t, err)
					require.Len(t, claims, 1)
					nextOwner = claims[0].RecoveryOwner
					require.NotEqual(t, current.RecoveryOwner, nextOwner)
				}
				return nativeresult.PollResult{Status: "running"}, nil
			})
			_, err := e.RecoverOnce(t.Context(), "same-worker", now, recoveryResolver(p), nil)
			if mode == "extended" {
				require.NoError(t, err)
				assertRecoveryOuter(t, e, "exited")
			} else {
				require.Error(t, err)
				assertRecoveryOuter(t, e, "accepted")
				task, e2 := model.GetNativeTask(e.DB, "recovery", "g", 1)
				require.NoError(t, e2)
				require.Equal(t, nextOwner, task.RecoveryOwner)
			}
		})
	}
}

func TestRecoveryHandoffReservedClaimRemainsUncovered(t *testing.T) {
	e, p, w, _ := setup(t)
	activateNativeRuntime(t, e, &p)
	installNativeInvocationJournal(t, e)
	installRecoveryWorkerHandoff(t, e)
	w.fail = "admit"
	_, err := e.Submit(t.Context(), "reserved", "g", 1, body, p)
	require.Error(t, err)
	task, err := model.GetNativeTask(e.DB, "reserved", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "reserved", task.Status)
	w.fail = ""
	report, err := e.RecoverOnce(t.Context(), "worker", time.Now(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Claimed)
	require.Empty(t, workerRecoveryHandoffs(t, e))
}
func TestRecoveryHandoffRecomputesSameStatusSchedule(t *testing.T) {
	e, _, _ := setupRecoveryHandoff(t, true)
	now := time.Now()
	require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "recovery").Updates(map[string]any{"status": model.NativeStatusDispatching, "dispatch_owner": "before", "dispatch_until": now.Add(20 * time.Second)}).Error)
	claims, err := model.ClaimNativeRecovery(e.DB, "worker", now, 1)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	task := claims[0]
	h, err := model.PrepareNativeRecoveryHandoff(t.Context(), e.DB, &task)
	require.NoError(t, err)
	accepted, err := h.Accept(t.Context())
	require.NoError(t, err)
	require.True(t, accepted)
	deadline := now.Add(time.Minute)
	require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", task.ID).Updates(map[string]any{"dispatch_owner": "after-round-trip", "dispatch_until": deadline}).Error)
	require.NoError(t, e.releaseRecoveryHandoff(task.ID, task.GroupID, task.TokenID, task.RecoveryOwner, &task, now, 0, false, false, time.Time{}, h))
	fresh, err := model.GetNativeTask(e.DB, task.ID, task.GroupID, task.TokenID)
	require.NoError(t, err)
	require.Equal(t, deadline.Add(time.Second).Unix(), fresh.NextRecoveryAt)
	require.Empty(t, fresh.RecoveryOwner)
	assertRecoveryOuter(t, e, "exited")
}

func TestRecoveryHandoffLateAllExtensionsDuringProofOutage(t *testing.T) {
	e, p, _, _ := setup(t)
	store, _ := activateNativeRuntime(t, e, &p)
	_, err := e.Submit(t.Context(), "recovery", "g", 1, body, p)
	require.NoError(t, err)
	installNativeInvocationJournal(t, e)
	installRecoveryWorkerHandoff(t, e)
	restore := model.ConfigureChannelAdmissionRuntime(store.Executor, nil)
	defer restore()
	poll := &pollStub{result: nativeresult.PollResult{Status: "running"}}
	report, err := e.RecoverOnce(t.Context(), "worker", time.Now(), recoveryResolver(poll), nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Advanced)
	require.Equal(t, 1, poll.calls)
	require.Empty(t, workerRecoveryHandoffs(t, e))
	require.Empty(t, nativeInvocations(t, e))
}
