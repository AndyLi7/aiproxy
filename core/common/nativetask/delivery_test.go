package nativetask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestNativeDeliveryRecoversPartialArchiveAndWallet(t *testing.T) {
	e, plan, w, p := setup(t)
	ctx := context.Background()
	var contract nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(plan.Contract, &contract))
	contract.Artifacts = []nativeresult.ArtifactBinding{{Path: []string{"files", "*", "url"}}}
	plan.Contract, _ = json.Marshal(contract)
	plan.DeliveryBase = "https://gateway.example"
	task, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	raw := []byte(`{"files":[{"url":"https://provider.example/a.svg"},{"url":"https://provider.example/b.zip"}],"seed":9007199254740993,"raw_svg":"<svg>data</svg>"}`)
	require.NoError(t, model.SaveNativeTaskResult(e.DB, task.ID, "g", 1, raw))
	calls := []int{}
	fail := true
	archive := func(_ context.Context, id string, index int, source string) (ownedartifact.Receipt, error) {
		calls = append(calls, index)
		if fail && index == 1 {
			return ownedartifact.Receipt{}, errors.New("storage unavailable")
		}
		return ownedartifact.Receipt{Key: fmt.Sprintf("native-results/%s/%d/%s.bin", id, index, strings.Repeat("a", 64)), SHA256: strings.Repeat("b", 64), Size: 23}, nil
	}
	_, err = e.Deliver(ctx, task.ID, "g", 1, archive)
	require.Error(t, err)
	persisted, err := model.GetNativeTask(e.DB, task.ID, "g", 1)
	require.NoError(t, err)
	require.Equal(t, "result_received", persisted.Status)
	require.Empty(t, persisted.DeliveredOutput)
	require.Error(t, model.PrepareNativeDelivery(e.DB, task.ID, "g", 1))
	fail = false
	w.fail = "delivered"
	_, err = e.Deliver(ctx, task.ID, "g", 1, archive)
	require.Error(t, err)
	persisted, err = model.GetNativeTask(e.DB, task.ID, "g", 1)
	require.NoError(t, err)
	require.Equal(t, "delivery_ready", persisted.Status)
	require.Equal(t, []int{0, 1, 1}, calls)
	require.Contains(t, persisted.DeliveredOutput, `9007199254740993`)
	require.NotContains(t, persisted.DeliveredOutput, "provider.example")
	require.Contains(t, persisted.DeliveredOutput, "https://gateway.example/v1/model-tasks/req/artifacts/1")
	w.fail = ""
	done, err := e.Deliver(ctx, task.ID, "g", 1, archive)
	require.NoError(t, err)
	require.Equal(t, "completed", done.Status)
	require.Equal(t, []int{0, 1, 1}, calls)
	require.Equal(t, 1, p.calls)
	_, err = e.Deliver(ctx, task.ID, "g", 2, archive)
	require.Error(t, err)
	require.Equal(t, []int{0, 1, 1}, calls)
}
func TestNativeDeliveryPreservesEmptySuccess(t *testing.T) {
	for _, raw := range []string{"null", "[]", "{}"} {
		t.Run(raw, func(t *testing.T) {
			e, plan, _, _ := setup(t)
			var c nativeresult.TaskContract
			require.NoError(t, json.Unmarshal(plan.Contract, &c))
			c.OutputSchema = json.RawMessage(`{}`)
			plan.Contract, _ = json.Marshal(c)
			task, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
			require.NoError(t, err)
			require.NoError(t, model.SaveNativeTaskResult(e.DB, task.ID, "g", 1, []byte(raw)))
			done, err := e.Deliver(context.Background(), task.ID, "g", 1, func(context.Context, string, int, string) (ownedartifact.Receipt, error) {
				t.Fatal("empty output must not download")
				return ownedartifact.Receipt{}, nil
			})
			require.NoError(t, err)
			require.Equal(t, "completed", done.Status)
			require.Equal(t, raw, done.DeliveredOutput)
		})
	}
}
