package main

import (
	"encoding/json"
	rv "github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestRealUnconstrainedJSONItemsSurviveGatewayMapping(t *testing.T) {
	raw, err := os.ReadFile("../../common/registryvalidation/testdata/provider-unknown-json-items.json")
	require.NoError(t, err)
	var cases []sample
	require.NoError(t, json.Unmarshal(raw, &cases))
	require.Len(t, cases, 1)
	f := cases[0]
	var c struct {
		Providers map[string]struct{ Upstream rv.ProviderSpec }
	}
	require.NoError(t, json.Unmarshal(f.Contract, &c))
	p := c.Providers["fal"].Upstream
	b := rv.ProviderBinding{Provider: "fal", ID: p.ID, Revision: p.Revision, ContractHash: p.ContractHash}
	for _, value := range []string{`[null,"text",42,true,{"nested":[1,null]},["x"]]`, `[]`, `null`} {
		var request map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(f.Request, &request))
		request["structured_instruction"] = json.RawMessage(`{"text_render":` + value + `}`)
		body, _ := json.Marshal(request)
		var model string
		require.NoError(t, json.Unmarshal(request["model"], &model))
		normalized, ve := rv.ValidateImage(f.Contract, model, body)
		require.Nil(t, ve)
		mapped, err := rv.MapBoundProviderInput(f.Contract, b, "fal-image", p.Endpoint, "async", normalized)
		require.NoError(t, err)
		var result map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(mapped, &result))
		require.JSONEq(t, string(request["structured_instruction"]), string(result["structured_instruction"]))
	}
	var request map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(f.Request, &request))
	request["structured_instruction"] = json.RawMessage(`{"text_render":"not an array"}`)
	body, _ := json.Marshal(request)
	var model string
	json.Unmarshal(request["model"], &model)
	_, ve := rv.ValidateImage(f.Contract, model, body)
	require.NotNil(t, ve)
}
