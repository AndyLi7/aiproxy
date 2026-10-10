package main

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestSourceProvenConditionalCounts(t *testing.T) {
	data, err := os.ReadFile("../../common/registryvalidation/testdata/conditional-count.json")
	require.NoError(t, err)
	var rows []sample
	require.NoError(t, json.Unmarshal(data, &rows))
	require.Len(t, rows, 28)
	for _, source := range rows {
		t.Run(source.Endpoint+"/"+source.Scenario, func(t *testing.T) {
			result := run(source)
			require.True(t, result.Passed, result.Error)
			require.True(t, result.OutputPassed, result.OutputError)
			// A single extra valid image must be refused, including automatic 40-image batches.
			row := source
			var response map[string]any
			require.NoError(t, json.Unmarshal(row.Response, &response))
			images := response["images"].([]any)
			response["images"] = append(images, images[0])
			row.Response, _ = json.Marshal(response)
			result = run(row)
			require.False(t, result.OutputPassed)
			// Invalid branch selectors must fail at the public contract, not reach a provider.
			row = source
			var request map[string]any
			require.NoError(t, json.Unmarshal(row.Request, &request))
			if _, ok := request["placement_type"]; ok {
				request["placement_type"] = "unknown"
			} else {
				request["result_type"] = "unknown"
			}
			row.Request, _ = json.Marshal(request)
			require.False(t, run(row).Passed)
		})
	}
}
