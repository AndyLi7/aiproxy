//nolint:testpackage // White-box coverage needs the package-private setup fixture or implementation.
package nativetask

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestIndependentNativeFrozenWorkAfterEpochActivation(t *testing.T) {
	for _, mode := range []string{"v1", "extension", "late-extension"} {
		for _, op := range []string{"replay", "poll", "billing", "recover", "delivery"} {
			t.Run(mode+"/"+op, func(t *testing.T) {
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

				_, err = store.Seal(
					t.Context(),
					model.ChannelSealRequest{
						OperationID: "move",
						Epoch:       "synthetic",
						From:        ids[0],
						To:          ids[1],
					},
				)
				require.NoError(t, err)

				for hop := int64(2); hop <= 3; hop++ {
					proof, err := store.LoadDeployment(t.Context())
					require.NoError(t, err)

					proof.ProofID = fmt.Sprintf("epoch-%d", hop)
					proof.Sequence = hop
					proof.Epoch = proof.ProofID
					executor := store.Executor
					executor.Epoch = proof.Epoch
					executor.InstanceID = fmt.Sprintf("instance-%d", hop)
					proof.Executors = []model.ChannelExecutorIdentity{executor}
					raw, err := json.Marshal(proof.Executors)
					require.NoError(t, err)

					sum := sha256.Sum256(raw)
					proof.MembershipHash = hex.EncodeToString(sum[:])
					store.Executor = executor
					store.LoadDeployment = func(context.Context) (model.VerifiedDeploymentProof, error) { return proof, nil }
					control, err := store.Activate(t.Context(), hop-1)
					require.NoError(t, err)
					require.Equal(t, hop, control.EpochSequence)

					restore := model.ConfigureChannelAdmissionRuntime(
						executor,
						store.LoadDeployment,
					)
					defer restore()

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
						task, err = model.GetNativeTask(e.DB, "frozen", "g", 1)
						require.NoError(t, err)
						err = e.SyncBilling(t.Context(), task)
					case "recover":
						require.NoError(
							t,
							e.DB.Model(&model.NativeTask{}).
								Where("id = ?", "frozen").
								Update("next_recovery_at", 0).
								Error,
						)

						var report RecoveryReport

						report, err = e.RecoverOnce(
							t.Context(),
							"worker",
							time.Now(),
							resolver,
							nil,
						)
						t.Logf("recovery report=%+v", report)
					case "delivery":
						if hop == 2 {
							require.NoError(
								t,
								model.SaveNativeTaskResult(
									e.DB,
									"frozen",
									"g",
									1,
									[]byte(`{"ok":true}`),
								),
							)
						}

						_, err = e.Deliver(t.Context(), "frozen", "g", 1, nil)
					}

					t.Logf(
						"mode=%s operation=%s activated_epoch=%s error=%v wallet_delta=%d poll_calls=%d provider_submits=%d",
						mode,
						op,
						control.Epoch,
						err,
						len(w.commands)-before,
						pollCalls,
						provider.calls,
					)
					require.NoError(t, err)
					require.Greater(t, len(w.commands), before)

					if op == "poll" || op == "recover" {
						require.Equal(t, 1, pollCalls)
					}

					require.Equal(t, 1, provider.calls)

					for _, action := range []string{"admit", "begin_attempt"} {
						count := 0
						for _, command := range w.commands {
							if command.Action == action {
								count++
							}
						}

						require.Equal(t, 1, count, action)
					}

					attemptID := ""
					for _, command := range w.commands {
						if command.Action == "begin_attempt" {
							attemptID = command.AttemptID
						}
					}

					require.NotEmpty(t, attemptID)

					for _, command := range w.commands {
						require.Equal(t, task.BillingOperationID, command.BillingOperationID)

						if command.Action == "accept_attempt" {
							require.Equal(t, attemptID, command.AttemptID)
						}
					}

					if mode != "v1" {
						for _, row := range nativeInvocations(t, e) {
							require.Equal(t, "unproven", row.ContinuationState)
						}
					}

					var uses []model.ChannelAdmissionUse
					require.NoError(t, e.DB.Find(&uses).Error)
					require.Len(t, uses, 1)
					require.Equal(t, "open", uses[0].State)
				}
			})
		}
	}
}
