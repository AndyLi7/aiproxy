package main

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestSourceMaximumImageCountsReachDelivery(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/count-boundaries.json")
	require.NoError(t, err)
	var cases []sample
	require.NoError(t, json.Unmarshal(raw, &cases))
	require.Len(t, cases, 5)
	for _, c := range cases {
		t.Run(c.Endpoint+"/"+c.Scenario, func(t *testing.T) {
			r := run(c)
			require.True(t, r.Passed, r.Error)
			require.True(t, r.OutputPassed, r.OutputError)
		})
	}
}
