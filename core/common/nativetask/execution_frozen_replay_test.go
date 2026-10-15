package nativetask

import (
	"context"
	"errors"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

// Main's actual Engine counterexample, extended with recovery/delivery and
// late installation. Billing always uses the latest persisted task snapshot.
func TestMainNativeExecutionFrozenWorkDuringProofOutage(t *testing.T) {
	for _, mode := range []string{"v1", "extension", "late-extension"} {
		for _, source := range []string{"missing", "unavailable"} {
			for _, op := range []string{"replay", "poll", "billing", "recover", "delivery"} {
				t.Run(mode+"/"+source+"/"+op, func(t *testing.T) {
					e, p, w, provider := setup(t)
					store, ids := activateNativeRuntime(t, e, &p)
					if mode == "extension" {
						installNativeInvocationJournal(t, e)
					}
					task, err := e.Submit(t.Context(), "frozen", "g", 1, body, p)
					require.NoError(t, err)
					require.Equal(t, "queued", task.Status)
					if mode == "late-extension" {
						installNativeInvocationJournal(t, e)
					}
					_, err = store.Seal(t.Context(), model.ChannelSealRequest{OperationID: "move", Epoch: "synthetic", From: ids[0], To: ids[1]})
					require.NoError(t, err)
					var load func(context.Context) (model.VerifiedDeploymentProof, error)
					if source == "unavailable" {
						load = func(context.Context) (model.VerifiedDeploymentProof, error) {
							return model.VerifiedDeploymentProof{}, errors.New("synthetic local snapshot unavailable")
						}
					}
					restore := model.ConfigureChannelAdmissionRuntime(store.Executor, load)
					defer restore()
					task, err = model.GetNativeTask(e.DB, "frozen", "g", 1)
					require.NoError(t, err)
					before := len(w.commands)
					pollCalls := 0
					resolver := func(context.Context, *model.NativeTask) (Poller, error) {
						pollCalls++
						return &pollStub{result: nativeresult.PollResult{Status: "running"}}, nil
					}
					switch op {
					case "replay":
						_, err = e.Submit(t.Context(), "frozen", "g", 1, body, p)
					case "poll":
						_, err = e.Poll(t.Context(), "frozen", "g", 1, resolver, nil)
					case "billing":
						err = e.SyncBilling(t.Context(), task)
					case "recover":
						var report RecoveryReport
						report, err = e.RecoverOnce(t.Context(), "worker", time.Now(), resolver, nil)
						require.Equal(t, 1, report.Claimed)
					case "delivery":
						require.NoError(t, model.SaveNativeTaskResult(e.DB, "frozen", "g", 1, []byte(`{"ok":true}`)))
						var delivered *model.NativeTask
						delivered, err = e.Deliver(t.Context(), "frozen", "g", 1, nil)
						require.NoError(t, err)
						require.Equal(t, "completed", delivered.Status)
					}
					t.Logf("mode=%s source=%s operation=%s error=%v wallet_delta=%d poll_calls=%d provider_submits=%d", mode, source, op, err, len(w.commands)-before, pollCalls, provider.calls)
					require.NoError(t, err)
					require.Greater(t, len(w.commands), before)
					if op == "poll" || op == "recover" {
						require.Equal(t, 1, pollCalls)
					}
					require.Equal(t, 1, provider.calls)
					beforeNew := len(w.commands)
					_, newErr := e.Submit(t.Context(), "new-after-seal", "g", 1, body, p)
					require.Error(t, newErr)
					require.Equal(t, beforeNew, len(w.commands))
					require.Equal(t, 1, provider.calls)
					if mode != "v1" {
						rows := nativeInvocations(t, e)
						if mode == "late-extension" {
							require.Empty(t, rows)
						} else {
							require.Greater(t, len(rows), 1)
						}
						for _, row := range rows {
							require.Equal(t, "unproven", row.ContinuationState)
						}
						// A later successful record cannot fill the unobserved historical gap.
						restoreProof := model.ConfigureChannelAdmissionRuntime(store.Executor, store.LoadDeployment)
						fresh, readErr := model.GetNativeTask(e.DB, "frozen", "g", 1)
						require.NoError(t, readErr)
						require.NoError(t, e.SyncBilling(t.Context(), fresh))
						restoreProof()
						for _, row := range nativeInvocations(t, e) {
							require.Equal(t, "unproven", row.ContinuationState)
						}
					}
					var uses []model.ChannelAdmissionUse
					require.NoError(t, e.DB.Find(&uses).Error)
					require.Len(t, uses, 1)
					require.Equal(t, "open", uses[0].State)
				})
			}
		}
	}
}
