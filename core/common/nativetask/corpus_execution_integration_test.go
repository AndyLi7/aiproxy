package nativetask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// Source contracts are real; provider, wallet and archive transports are synthetic.
// Exercises the durable engine, not paid execution or actual bill acceptance.
func TestNativeCorpusExecution(t *testing.T) {
	path := os.Getenv("FAL_NATIVE_EXECUTION_CASES")
	if path == "" {
		t.Skip("requires exact 66-endpoint exported cases")
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var cases []struct {
		Endpoint   string          `json:"endpoint"`
		SourceHash string          `json:"sourceHash"`
		Contract   json.RawMessage `json:"contract"`
		Input      json.RawMessage `json:"input"`
		Output     json.RawMessage `json:"output"`
		Error      string          `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &cases))
	require.Len(t, cases, 66)
	seen := map[string]bool{}
	evidence := []map[string]any{}
	for _, c := range cases {
		require.False(t, seen[c.Endpoint])
		seen[c.Endpoint] = true
		passed := t.Run(c.Endpoint, func(t *testing.T) {
			require.Empty(t, c.Error)
			require.Len(t, c.SourceHash, 64)
			e, plan, w, p := setup(t)
			sql, err := e.DB.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = sql.Close() })
			plan.Contract = c.Contract
			plan.Endpoint = c.Endpoint
			plan.DeliveryBase = "https://gateway.example"
			var quote map[string]any
			require.NoError(t, json.Unmarshal([]byte(plan.QuoteJSON), &quote))
			quote["prepaidMicros"] = 100000
			route := quote["routes"].([]any)[0].(map[string]any)
			route["endpoint"] = c.Endpoint
			route["prepaidMicros"] = 100000
			route["estimatedMicros"] = 100000
			frozenQuote, err := json.Marshal(quote)
			require.NoError(t, err)
			plan.QuoteJSON = string(frozenQuote)
			var contract nativeresult.TaskContract
			require.NoError(t, json.Unmarshal(c.Contract, &contract))
			compiled, err := nativeresult.CompileTaskContract(c.Contract)
			require.NoError(t, err)
			body, err := json.Marshal(map[string]any{"model": contract.Model, "input": c.Input})
			require.NoError(t, err)
			prepared, err := compiled.PrepareUpstreamInput(c.Input)
			require.NoError(t, err, "synthetic input must match real schema")
			_, err = compiled.ValidateOutput(c.Output)
			require.NoError(t, err, "synthetic output must match real schema")
			ctx := context.Background()
			task, err := e.Submit(ctx, "corpus", "owner", 1, body, plan)
			require.NoError(t, err)
			require.Equal(t, "queued", task.Status)
			require.JSONEq(t, string(prepared), string(p.body))
			require.Equal(t, []string{"admit", "begin_attempt", "accept_attempt"}, []string{w.commands[0].Action, w.commands[1].Action, w.commands[2].Action})
			_, err = e.Submit(ctx, "corpus", "owner", 1, body, plan)
			require.NoError(t, err)
			require.Equal(t, 1, p.calls)
			_, err = e.Submit(ctx, "corpus", "owner", 2, body, plan)
			require.Error(t, err)
			require.NoError(t, model.SaveNativeTaskResult(e.DB, task.ID, "owner", 1, c.Output))
			artifacts, err := nativeresult.PlanArtifacts(c.Output, contract.Artifacts)
			require.NoError(t, err)
			calls := 0
			failLast := len(artifacts) > 0
			archive := func(_ context.Context, id string, index int, source string) (ownedartifact.Receipt, error) {
				calls++
				if failLast && index == len(artifacts)-1 {
					failLast = false
					return ownedartifact.Receipt{}, errors.New("synthetic archive interruption")
				}
				require.Equal(t, artifacts[index].Source, source)
				return ownedartifact.Receipt{Key: fmt.Sprintf("native-results/%s/%d/%s.bin", id, index, strings.Repeat("a", 64)), SHA256: strings.Repeat("b", 64), Size: 123}, nil
			}
			// A new Engine instance must finish from durable state with no new submission.
			recovered := &Engine{DB: e.DB, Wallet: w, Provider: p}
			if len(artifacts) > 0 {
				_, err = recovered.Deliver(ctx, task.ID, "owner", 1, archive)
				require.Error(t, err)
				pending, err := model.GetNativeTask(e.DB, task.ID, "owner", 1)
				require.NoError(t, err)
				require.Equal(t, "result_received", pending.Status)
				require.Empty(t, pending.DeliveredOutput)
			}
			done, err := recovered.Deliver(ctx, task.ID, "owner", 1, archive)
			require.NoError(t, err)
			require.Equal(t, "completed", done.Status)
			expectedCalls := len(artifacts)
			if len(artifacts) > 0 {
				expectedCalls++
			}
			require.Equal(t, expectedCalls, calls)
			_, err = compiled.ValidateOutput([]byte(done.DeliveredOutput))
			require.NoError(t, err)
			delivered, err := nativeresult.PlanArtifacts([]byte(done.DeliveredOutput), contract.Artifacts)
			require.NoError(t, err)
			for _, a := range delivered {
				require.True(t, strings.HasPrefix(a.Source, plan.DeliveryBase+"/v1/model-tasks/"))
			}
			_, err = recovered.Deliver(ctx, task.ID, "owner", 1, archive)
			require.NoError(t, err)
			require.Equal(t, expectedCalls, calls)
			_, err = recovered.Deliver(ctx, task.ID, "other-owner", 1, archive)
			require.Error(t, err)
			require.Equal(t, 1, p.calls)
		})
		evidence = append(evidence, map[string]any{"endpoint": c.Endpoint, "sourceHash": c.SourceHash, "isolatedEnginePassed": passed, "providerSynthetic": true, "walletSynthetic": true, "archiveSynthetic": true, "paidGenerationVerified": false, "billingAcceptanceVerified": false})
	}
	if out := os.Getenv("FAL_NATIVE_EXECUTION_REPORT"); out != "" {
		report, err := json.MarshalIndent(evidence, "", "  ")
		require.NoError(t, err)
		file, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		require.NoError(t, err)
		_, err = file.Write(report)
		require.NoError(t, err)
		require.NoError(t, file.Close())
	}
}
