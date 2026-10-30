package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func wanEditContract(t *testing.T) (json.RawMessage, string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/wan-edit-tools/wan26-edit.json")
	require.NoError(t, err)
	var revision struct {
		Contract json.RawMessage `json:"contract"`
		Source   struct {
			Endpoint string `json:"endpoint"`
		} `json:"source"`
	}
	require.NoError(t, json.Unmarshal(raw, &revision))
	return revision.Contract, revision.Source.Endpoint
}

func TestToolsFalReferencesUseOfficialFileMetadataWithoutRelaxingAPI(t *testing.T) {
	raw, _ := wanEditContract(t)
	before := string(raw)
	require.True(t, toolsFalDataURIReferences(raw))
	var contract nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(raw, &contract))
	require.False(t, agentDataURIReferences(contract.InputSchema), "public API admission is unchanged")
	require.Equal(t, before, string(raw), "frozen contract is immutable")
	for _, tc := range []string{"no-image-metadata", "https-only", "root-rejects-images", "upstream-https-only"} {
		t.Run(tc, func(t *testing.T) {
			var c map[string]any
			require.NoError(t, json.Unmarshal(raw, &c))
			key := "input_schema"
			if tc == "upstream-https-only" {
				key = "upstream_input_schema"
			}
			root := c[key].(map[string]any)
			if tc == "upstream-https-only" {
				root = root["components"].(map[string]any)["schemas"].(map[string]any)["V26ImageToImageInput"].(map[string]any)
			}
			field := root["properties"].(map[string]any)["image_urls"].(map[string]any)
			item := field["items"].(map[string]any)
			switch tc {
			case "no-image-metadata":
				delete(item, "_fal_ui_field")
			case "https-only", "upstream-https-only":
				item["pattern"] = "^https://"
			case "root-rejects-images":
				root["allOf"] = []any{map[string]any{"properties": map[string]any{"image_urls": map[string]any{"maxItems": 0}}}}
			}
			changed, _ := json.Marshal(c)
			require.False(t, toolsFalDataURIReferences(changed))
		})
	}
}

func TestToolsWanEditBindingPassesReferenceBytesToQueueWithoutAPIOrRetail(t *testing.T) {
	r, db, legacy := toolsControllerSetup(t)
	r.PUT("/binding", PutToolsModelBinding)
	raw, endpoint := wanEditContract(t)
	var contract nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(raw, &contract))
	base, ok := parseNativeRouteBinding(legacy)
	require.True(t, ok)
	base.Contract, base.Endpoint = raw, endpoint
	input := toolsBindingInput{Model: contract.Model, Capability: "image-to-image", Enabled: true, Binding: base, Source: toolsBindingSource{ModelID: "model", CapabilityID: "cap", DraftID: "draft", DraftVersion: 2, ChannelID: "fal"}}
	send := func(req *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	body, _ := json.Marshal(input)
	w := send(httptest.NewRequest("PUT", "/binding", strings.NewReader(string(body))))
	require.Equal(t, 200, w.Code, w.Body.String())
	access := send(signedToolsRequest(t, "GET", "/internal/tools/model-access?model="+contract.Model, "", "access", "", "owner"))
	require.Equal(t, 200, access.Code, access.Body.String())
	var descriptor struct {
		Revision string `json:"revision"`
		Agent    struct {
			ReferenceFormat string `json:"referenceFormat"`
			ReferenceField  string `json:"referenceField"`
		} `json:"agent"`
	}
	require.NoError(t, json.Unmarshal(access.Body.Bytes(), &descriptor))
	require.Equal(t, "data-uri-array", descriptor.Agent.ReferenceFormat)
	require.Equal(t, "image_urls", descriptor.Agent.ReferenceField)
	calls := 0
	transport := http.DefaultTransport
	http.DefaultTransport = toolsRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Method != "POST" || req.URL.Host != "queue.fal.run" || req.URL.Path != "/"+endpoint {
			return nil, errors.New("external traffic forbidden")
		}
		calls++
		var received map[string]any
		require.NoError(t, json.NewDecoder(req.Body).Decode(&received))
		require.Equal(t, []any{toolsPNGReferenceProbe}, received["image_urls"])
		require.Equal(t, true, received["enable_safety_checker"])
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"request_id":"synthetic-edit"}`)), Header: make(http.Header)}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = transport })
	request, _ := json.Marshal(map[string]any{"model": contract.Model, "input": map[string]any{"prompt": "Change the flower color only", "image_urls": []string{toolsPNGReferenceProbe}, "num_images": 1}})
	created := send(signedToolsRequest(t, "POST", "/internal/tools/model-tasks", string(request), "tools_edit", descriptor.Revision, "owner"))
	require.Equal(t, 202, created.Code, created.Body.String())
	require.Equal(t, 1, calls)
	replay := send(signedToolsRequest(t, "POST", "/internal/tools/model-tasks", string(request), "tools_edit", descriptor.Revision, "owner"))
	require.Equal(t, 202, replay.Code)
	require.Equal(t, 1, calls, "replay must not submit another edit")
	// RECONSTRUCTED uses a separate Tools table, never the retail NativeTask ledger.
	var task model.ToolsExecution
	require.NoError(t, db.First(&task, "id = ?", "tools_edit").Error)
	require.Equal(t, "pending", task.Status)
	var count int64
	require.NoError(t, db.Model(&model.ModelConfig{}).Where("model = ?", contract.Model).Count(&count).Error)
	require.Zero(t, count, "no public API model created")
	require.NotContains(t, model.LoadModelCaches().EnabledModelConfigsMap, contract.Model)
	invalid := `{"model":"` + contract.Model + `","input":{"prompt":"missing reference"}}`
	denied := send(signedToolsRequest(t, "POST", "/internal/tools/model-tasks", invalid, "tools_invalid", descriptor.Revision, "owner"))
	require.Equal(t, 422, denied.Code)
	require.Equal(t, 1, calls, "invalid request must fail before provider submission")
}
