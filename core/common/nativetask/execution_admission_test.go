package nativetask

import (
	"context"
	"encoding/json"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func installNativeInvocationJournal(t *testing.T, e *Engine) {
	t.Helper()
	require.NoError(t, e.DB.AutoMigrate(&model.ChannelAdmissionExecutionProtocol{}, &model.ChannelAdmissionExecution{}, &model.ChannelAdmissionTaskExecution{}))
	require.NoError(t, e.DB.Create(&[]model.ChannelAdmissionExecutionProtocol{{ID: 1, Version: 1}, {ID: 2, Version: 1}}).Error)
}
func nativeInvocations(t *testing.T, e *Engine) []model.ChannelAdmissionTaskExecution {
	t.Helper()
	var rows []model.ChannelAdmissionTaskExecution
	require.NoError(t, e.DB.Find(&rows).Error)
	return rows
}

type executionWallet struct {
	t      *testing.T
	e      *Engine
	wallet Wallet
	hook   func(balance.PrepaymentCommand)
}

func (w *executionWallet) Prepayment(ctx context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	active := false
	for _, row := range nativeInvocations(w.t, w.e) {
		if row.State == "active" {
			active = true
		}
	}
	require.True(w.t, active, "wallet call must have an active invocation")
	if w.hook != nil {
		w.hook(c)
	}
	return w.wallet.Prepayment(ctx, c)
}

type executionProvider struct {
	t        *testing.T
	e        *Engine
	provider Provider
	hook     func()
}

func (p *executionProvider) SubmitNative(ctx context.Context, endpoint string, body []byte) (string, error) {
	active := false
	for _, row := range nativeInvocations(p.t, p.e) {
		if row.State == "active" && (row.Kind == "native_submit" || row.Kind == "native_dispatch") {
			active = true
		}
	}
	require.True(p.t, active)
	if p.hook != nil {
		p.hook()
	}
	return p.provider.SubmitNative(ctx, endpoint, body)
}
func TestNativeInvocationActualSubmitPollDeliveryAndBilling(t *testing.T) {
	e, plan, w, p := setup(t)
	activateNativeRuntime(t, e, &plan)
	installNativeInvocationJournal(t, e)
	e.Wallet = &executionWallet{t: t, e: e, wallet: w}
	e.Provider = &executionProvider{t: t, e: e, provider: p}
	task, err := e.Submit(t.Context(), "fact", "g", 1, body, plan)
	require.NoError(t, err)
	require.Equal(t, "queued", task.Status)
	rows := nativeInvocations(t, e)
	require.Len(t, rows, 1)
	require.Equal(t, "returned", rows[0].State)
	poll := &pollStub{result: nativeresult.PollResult{Status: "result_received", Output: json.RawMessage(`{"ok":true}`)}}
	done, err := e.Poll(t.Context(), "fact", "g", 1, func(context.Context, *model.NativeTask) (Poller, error) {
		// The nested billing call returned; the outer poll must still be active.
		activePoll, returnedBilling := false, false
		for _, r := range nativeInvocations(t, e) {
			if r.Kind == "native_poll" && r.State == "active" {
				activePoll = true
			}
			if r.Kind == "native_billing" && r.State == "returned" {
				returnedBilling = true
			}
		}
		require.True(t, activePoll)
		require.True(t, returnedBilling)
		return poll, nil
	}, nil)
	require.NoError(t, err)
	require.Equal(t, "completed", done.Status)
	require.Equal(t, 1, p.calls)
	kinds := map[string]bool{}
	for _, row := range nativeInvocations(t, e) {
		kinds[row.Kind] = true
		require.Equal(t, "returned", row.State)
		require.Empty(t, row.RecoveryOwner)
		require.Equal(t, "unproven", row.ContinuationState)
	}
	for _, kind := range []string{"native_submit", "native_poll", "native_delivery", "native_billing"} {
		require.True(t, kinds[kind], kind)
	}
	var use model.ChannelAdmissionUse
	require.NoError(t, e.DB.First(&use).Error)
	require.Equal(t, "open", use.State)
}
func TestNativeInvocationActualPanicAndOwnerChangeRetain(t *testing.T) {
	for _, scenario := range []string{"provider-panic", "recovery-owner-change", "partial-storage"} {
		t.Run(scenario, func(t *testing.T) {
			e, plan, w, p := setup(t)
			activateNativeRuntime(t, e, &plan)
			installNativeInvocationJournal(t, e)
			if scenario == "partial-storage" {
				require.NoError(t, e.DB.Where("id = ?", 2).Delete(&model.ChannelAdmissionExecutionProtocol{}).Error)
				_, err := e.Submit(t.Context(), "fact", "g", 1, body, plan)
				require.Error(t, err)
				require.Empty(t, w.commands)
				require.Zero(t, p.calls)
				return
			}
			if scenario == "provider-panic" {
				e.Provider = &executionProvider{t: t, e: e, provider: p, hook: func() { panic("synthetic crash") }}
				require.Panics(t, func() { e.Submit(t.Context(), "fact", "g", 1, body, plan) })
				require.Equal(t, "active", nativeInvocations(t, e)[0].State)
				return
			}
			_, err := e.Submit(t.Context(), "fact", "g", 1, body, plan)
			require.NoError(t, err)
			require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "fact").Updates(map[string]any{"recovery_owner": "owner", "recovery_until": time.Now().Add(time.Minute).Unix()}).Error)
			task, err := model.GetNativeTask(e.DB, "fact", "g", 1)
			require.NoError(t, err)
			e.Wallet = &executionWallet{t: t, e: e, wallet: w, hook: func(c balance.PrepaymentCommand) {
				if c.Action == "get" {
					require.NoError(t, e.DB.Model(&model.NativeTask{}).Where("id = ?", "fact").Update("recovery_owner", "replacement-owner").Error)
				}
			}}
			require.NoError(t, e.SyncBilling(context.WithValue(context.WithValue(t.Context(), nativeRecoveryOwnerKey{}, "owner"), nativeRecoveryExecutionUntilKey{}, task.RecoveryUntil), task))
			active := 0
			for _, r := range nativeInvocations(t, e) {
				if r.Kind == "native_billing" {
					require.Equal(t, "active", r.State)
					require.Equal(t, "owner", r.RecoveryOwner)
					active++
				}
			}
			require.Equal(t, 1, active)
		})
	}
}
func TestNativeInvocationActualDispatchKeepsExistingOwner(t *testing.T) {
	e, plan, _, p := setup(t)
	activateNativeRuntime(t, e, &plan)
	installNativeInvocationJournal(t, e)
	// The normal enqueue fast path claims and dispatches via the same APIs.
	queue = newQueueState()
	require.NoError(t, e.DB.AutoMigrate(&model.ImageTask{}))
	wallet := newQueueWallet()
	e.Wallet = &executionWallet{t: t, e: e, wallet: wallet}
	e.Provider = &executionProvider{t: t, e: e, provider: p}
	e.Queue = &QueueConfig{Enabled: func() bool { return true }}
	result, err := e.Submit(t.Context(), "queue-fact", "g", 1, body, plan)
	require.NoError(t, err)
	require.Equal(t, "queued", result.Status)
	require.Equal(t, 1, p.calls)
	count := 0
	for _, row := range nativeInvocations(t, e) {
		if row.Kind == "native_dispatch" {
			count++
			require.NotEmpty(t, row.DispatchOwner)
			require.Equal(t, "returned", row.State)
			require.Equal(t, "unproven", row.ContinuationState)
		}
	}
	require.Equal(t, 1, count)
}

func TestNativeInvocationRecoverOnceCleanupCannotReleaseSameWorkerNewClaim(t *testing.T) {
	for _, extension := range []bool{false, true} {
		t.Run(map[bool]string{false: "v1", true: "execution-extension"}[extension], func(t *testing.T) {
			e, plan, w, _ := setup(t)
			activateNativeRuntime(t, e, &plan)
			if extension {
				installNativeInvocationJournal(t, e)
			}
			_, err := e.Submit(t.Context(), "same-worker", "g", 1, body, plan)
			require.NoError(t, err)
			now := time.Now().Truncate(time.Second)
			var old, newer *model.NativeTask
			wrapper := &reclaimDuringGetWallet{Wallet: w}
			wrapper.hook = func() {
				old, err = model.GetNativeTask(e.DB, "same-worker", "g", 1)
				require.NoError(t, err)
				require.Len(t, old.RecoveryOwner, 64)
				require.NoError(t, model.ReleaseNativeRecovery(e.DB, old.ID, old.RecoveryOwner, now))
				claims, e2 := model.ClaimNativeRecovery(e.DB, "same-worker-label", now, 1)
				require.NoError(t, e2)
				require.Len(t, claims, 1)
				newer = &claims[0]
				require.Equal(t, old.RecoveryUntil, newer.RecoveryUntil)
				require.NotEqual(t, old.RecoveryOwner, newer.RecoveryOwner)
			}
			e.Wallet = wrapper
			_, err = e.RecoverOnce(t.Context(), "same-worker-label", now, func(context.Context, *model.NativeTask) (Poller, error) {
				return &pollStub{result: nativeresult.PollResult{Status: "running"}}, nil
			}, nil)
			require.Error(t, err, "old cleanup must fail against a new claim")
			require.NotNil(t, newer)
			current, err := model.GetNativeTask(e.DB, "same-worker", "g", 1)
			require.NoError(t, err)
			require.Equal(t, newer.RecoveryOwner, current.RecoveryOwner)
			require.Equal(t, newer.RecoveryUntil, current.RecoveryUntil)
			if extension {
				active := 0
				for _, row := range nativeInvocations(t, e) {
					if row.RecoveryOwner == old.RecoveryOwner {
						require.Equal(t, "active", row.State)
						active++
					}
				}
				require.Greater(t, active, 0)
			}
		})
	}
}

type reclaimDuringGetWallet struct {
	Wallet
	hook func()
}

func (w *reclaimDuringGetWallet) Prepayment(ctx context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	if c.Action == "get" && w.hook != nil {
		hook := w.hook
		w.hook = nil
		hook()
	}
	return w.Wallet.Prepayment(ctx, c)
}
