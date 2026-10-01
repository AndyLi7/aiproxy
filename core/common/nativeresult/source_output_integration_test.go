package nativeresult

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// Actual exported source contracts with synthetic results; no provider network.
func TestNativeSourceOutputAlternatives(t *testing.T) {
	file := os.Getenv("FAL_NATIVE_OUTPUT_CASES")
	if file == "" {
		t.Skip("requires exported source/output cases")
	}
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	var cases []struct {
		Endpoint      string          `json:"endpoint"`
		Contract      json.RawMessage `json:"contract"`
		Output        json.RawMessage `json:"output"`
		ArtifactCount int             `json:"artifactCount"`
	}
	require.NoError(t, json.Unmarshal(raw, &cases))
	require.Len(t, cases, 10)
	for i, c := range cases {
		t.Run(fmt.Sprintf("%s/%d", c.Endpoint, i), func(t *testing.T) {
			compiled, err := CompileTaskContract(c.Contract)
			require.NoError(t, err)
			output, err := compiled.ValidateOutput(c.Output)
			require.NoError(t, err)
			var contract TaskContract
			require.NoError(t, json.Unmarshal(c.Contract, &contract))
			plan, err := PlanArtifacts(output, contract.Artifacts)
			require.NoError(t, err)
			require.Len(t, plan, c.ArtifactCount)
			owned := map[string]string{}
			for i, artifact := range plan {
				owned[artifact.Pointer] = fmt.Sprintf("https://gateway.example/v1/model-tasks/test/artifacts/%d", i)
			}
			delivered, err := RewriteArtifacts(output, contract.Artifacts, owned)
			require.NoError(t, err)
			require.NotContains(t, string(delivered), "https://provider.example")
			_, err = compiled.ValidateOutput(delivered)
			require.NoError(t, err)
			if len(plan) > 0 {
				delete(owned, plan[0].Pointer)
				_, err = RewriteArtifacts(output, contract.Artifacts, owned)
				require.Error(t, err, "partial archive cannot be delivered")
			}
		})
	}
}
