package nativetask

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// publicIDBody calls the native test contract (model brand/model/native) by
// the ID the gateway resolved to it.
var publicIDBody = []byte(`{"model":"brand/model","input":{"seed":9007199254740993}}`)

type publicTaskBody struct {
	ID     string `json:"id"`
	Model  string `json:"model"`
	Status string `json:"status"`
}

func decodePublicTask(t *testing.T, raw []byte) publicTaskBody {
	t.Helper()
	var task publicTaskBody
	require.NoError(t, json.Unmarshal(raw, &task))
	return task
}

// A request by the callable ID creates the same task as one by the capability
// ID: the stored task, wallet model and contract keep the capability ID; the
// response names the callable ID (owner decision D2). The fingerprint is over
// the original bytes, so a retry with the same body is idempotent and the
// other spelling under the same X-Request-Id is a conflict.
func TestNativeHTTPAcceptedPublicIDIdempotency(t *testing.T) {
	e, plan, wallet, p := setup(t)
	plan.AcceptedModel = "brand/model"
	h := &HTTP{Engine: e, Identity: func(*http.Request) (string, int, error) { return "g", 1, nil },
		ResolvePlan: func(*http.Request, []byte) (Plan, Provider, error) { return plan, p, nil },
		PublicModel: func(id string) string {
			if id == "brand/model/native" {
				return "brand/model"
			}
			return id
		}}

	response := httptest.NewRecorder()
	h.Create(response, request(publicIDBody))
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	require.Equal(t, "brand/model", decodePublicTask(t, response.Body.Bytes()).Model)
	require.Equal(t, 1, p.calls)
	require.Equal(t, `{"seed":9007199254740993}`, string(p.body))

	stored, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "brand/model/native", stored.Model)
	require.Equal(t, "brand/model/native", wallet.commands[0].ModelID)

	// Same ID and body: the original task, not a second submission.
	response = httptest.NewRecorder()
	h.Create(response, request(publicIDBody))
	require.Equal(t, http.StatusAccepted, response.Code)
	require.Equal(t, "brand/model", decodePublicTask(t, response.Body.Bytes()).Model)
	require.Equal(t, 1, p.calls)

	// Same ID, other spelling of the model: a different request body.
	response = httptest.NewRecorder()
	h.Create(response, request(body))
	require.Equal(t, http.StatusConflict, response.Code)
	require.Equal(t, "request_id_conflict", decodeNativeError(t, response.Body.Bytes()).Error.Code)
	require.Equal(t, 1, p.calls)

	// Reads report the callable ID too.
	response = httptest.NewRecorder()
	h.Get(response, httptest.NewRequest(http.MethodGet, "/v1/model-tasks/req", nil), "req")
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "brand/model", decodePublicTask(t, response.Body.Bytes()).Model)
}

// Without the gateway's resolution (AcceptedModel unset) another ID never
// passes the contract.
func TestNativeHTTPUnresolvedOtherIDIsInvalidInput(t *testing.T) {
	e, plan, _, p := setup(t)
	h := &HTTP{Engine: e, Identity: func(*http.Request) (string, int, error) { return "g", 1, nil },
		ResolvePlan: func(*http.Request, []byte) (Plan, Provider, error) { return plan, p, nil }}
	response := httptest.NewRecorder()
	h.Create(response, request(publicIDBody))
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Equal(t, "invalid_input", decodeNativeError(t, response.Body.Bytes()).Error.Code)
	require.Zero(t, p.calls)
}

func TestNativeWritePublicMapsModelOrKeepsIt(t *testing.T) {
	task := &model.NativeTask{ID: "t", Model: "brand/model/native", Status: "queued"}
	for _, tc := range []struct {
		name string
		hook func(string) string
		want string
	}{
		{"no hook", nil, "brand/model/native"},
		{"mapped", func(string) string { return "brand/model" }, "brand/model"},
		{"unknown config", func(id string) string { return id }, "brand/model/native"},
		{"empty answer", func(string) string { return "" }, "brand/model/native"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			(&HTTP{PublicModel: tc.hook}).writePublic(recorder, http.StatusOK, task)
			require.Equal(t, tc.want, decodePublicTask(t, recorder.Body.Bytes()).Model)
		})
	}
	// The exported writer (private trial evidence) keeps the stored ID.
	recorder := httptest.NewRecorder()
	WritePublic(recorder, http.StatusOK, task)
	require.Equal(t, "brand/model/native", decodePublicTask(t, recorder.Body.Bytes()).Model)
}

type nativeModelErrorBody struct {
	Error struct {
		Code            string    `json:"code"`
		Message         string    `json:"message"`
		Type            string    `json:"type"`
		Param           string    `json:"param"`
		SuggestedModels *[]string `json:"suggested_models"`
	} `json:"error"`
}

func decodeNativeError(t *testing.T, raw []byte) nativeErrorBody {
	t.Helper()
	var decoded nativeErrorBody
	require.NoError(t, json.Unmarshal(raw, &decoded))
	return decoded
}

func TestNativeModelErrorsCarrySuggestedModels(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteErrorDetail(recorder, http.StatusNotFound, "model_not_found", "model", nil)
	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.JSONEq(t, `{"error":{"code":"model_not_found","type":"not_found_error","param":"model","suggested_models":[],
		"message":"This ID cannot be called with this API key. Do not resend it: use an ID from suggested_models or an `+"`id`"+` from GET /v1/models."}}`,
		recorder.Body.String())

	// A model published on another endpoint names the IDs to use there.
	e, _, _, _ := setup(t)
	var seen []string
	h := &HTTP{Engine: e, Identity: func(*http.Request) (string, int, error) { return "g", 1, nil },
		OnError: func(_ int, code, _ string) { seen = append(seen, code) },
		ResolvePlan: func(*http.Request, []byte) (Plan, Provider, error) {
			return Plan{}, nil, &NotNativeModelError{SuggestedModels: []string{"brand/image/text-to-image"}}
		}}
	recorder = httptest.NewRecorder()
	h.Create(recorder, request(body))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	var decoded nativeModelErrorBody
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &decoded))
	require.Equal(t, "native_model_unavailable", decoded.Error.Code)
	require.Equal(t, "invalid_request_error", decoded.Error.Type)
	require.Equal(t, "model", decoded.Error.Param)
	require.Equal(t, errorMessages["native_model_unavailable"], decoded.Error.Message)
	require.Equal(t, &[]string{"brand/image/text-to-image"}, decoded.Error.SuggestedModels)
	require.Equal(t, []string{"native_model_unavailable"}, seen)
}
