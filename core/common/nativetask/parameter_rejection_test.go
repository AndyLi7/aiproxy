package nativetask

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"github.com/stretchr/testify/require"
)

// falVoiceRejection is fal's model error for an unsupported voice. msg, input,
// url and ctx carry free text, the customer's value and supplier identity.
const falVoiceRejection = `{"detail":[{"loc":["body","voice"],"msg":"Voice not found: NoSuchVoice123","type":"feature_not_supported","url":"https://docs.fal.ai/errors#feature_not_supported","input":"NoSuchVoice123","ctx":{"vendor":"elevenlabs"}}]}`

var falTextLeaks = []string{"NoSuchVoice123", "Voice not found", "docs.fal.ai", "elevenlabs", "fal-native-secret-key"}

type publicFailure struct {
	Status string `json:"status"`
	Error  struct {
		Code    string                        `json:"code"`
		Message string                        `json:"message"`
		Issues  []nativeresult.ParameterIssue `json:"issues"`
	} `json:"error"`
}

const invalidParametersMessage = "The provider rejected the listed input parameters. You were not charged; fix them and submit a new task with a new X-Request-Id."

func requireVoiceRejection(t *testing.T, raw []byte) {
	t.Helper()
	var got publicFailure
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, "failed", got.Status)
	require.Equal(t, "invalid_parameters", got.Error.Code)
	require.Equal(t, invalidParametersMessage, got.Error.Message)
	require.Equal(t, []nativeresult.ParameterIssue{{Field: "voice", Rule: "unsupported_value"}}, got.Error.Issues)
	for _, leak := range falTextLeaks {
		require.NotContains(t, string(raw), leak)
	}
}

// Owner decision 2026-10-09, submit-time path: fal refuses the input with a
// named field. The customer sees the field and rule, never fal's text, and the
// hold is refunded exactly like upstream_rejected (not accepted).
func TestNativeSubmitRejectionNamesFieldsAndRefundsNotAccepted(t *testing.T) {
	hook := captureLogs(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(falVoiceRejection))
	}))
	defer server.Close()
	e, plan, wallet, _ := setup(t)
	provider := &fal.Client{HTTP: server.Client(), BaseURL: server.URL, Key: "fal-native-secret-key"}
	h := &HTTP{Engine: e, Identity: func(*http.Request) (string, int, error) { return "g", 1, nil },
		ResolvePlan: func(*http.Request, []byte) (Plan, Provider, error) { return plan, provider, nil }}

	recorder := httptest.NewRecorder()
	h.Create(recorder, request(body))
	require.Equal(t, http.StatusAccepted, recorder.Code)
	requireVoiceRejection(t, recorder.Body.Bytes())
	require.Equal(t, []string{"admit", "begin_attempt", "reject_attempt", "settle:failed/not_accepted"}, walletActions(wallet))

	stored, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "failed", stored.Status)
	require.Empty(t, stored.UpstreamID)
	require.JSONEq(t, `{"issues":[{"field":"voice","rule":"unsupported_value"}]}`, stored.PublicError)

	// Reads and replays report the stored issues.
	recorder = httptest.NewRecorder()
	h.Get(recorder, httptest.NewRequest(http.MethodGet, "/v1/model-tasks/req", nil), "req")
	require.Equal(t, http.StatusOK, recorder.Code)
	requireVoiceRejection(t, recorder.Body.Bytes())
	recorder = httptest.NewRecorder()
	h.Create(recorder, request(body))
	require.Equal(t, http.StatusAccepted, recorder.Code)
	requireVoiceRejection(t, recorder.Body.Bytes())

	// Recovery replays the same not-accepted refund, never platform_failure.
	wallet.commands = nil
	require.NoError(t, e.SyncBilling(context.Background(), stored))
	require.Equal(t, []string{"reject_attempt", "settle:failed/not_accepted", "get"}, walletActions(wallet))

	entry := logEntry(t, hook, "native task provider submission failed")
	require.Equal(t, 422, entry.Data["provider_status"])
	require.Equal(t, "feature_not_supported@body.voice", entry.Data["provider_reason"])
	require.Equal(t, "invalid_parameters", entry.Data["error_code"])
	requireNoSecretsLogged(t, hook, falTextLeaks...)
}

// A rejection without named fields keeps upstream_rejected.
func TestNativeSubmitRejectionWithoutFieldsStaysUpstreamRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":"Voice not found: NoSuchVoice123","error_type":"request_error"}`))
	}))
	defer server.Close()
	e, plan, wallet, _ := setup(t)
	e.Provider = &fal.Client{HTTP: server.Client(), BaseURL: server.URL, Key: "fal-native-secret-key"}
	task, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.Equal(t, "upstream_rejected", task.ErrorCode)
	require.Empty(t, task.PublicError)
	require.Equal(t, "settle:failed/not_accepted", walletActions(wallet)[3])
	recorder := httptest.NewRecorder()
	WritePublic(recorder, http.StatusOK, task)
	require.NotContains(t, recorder.Body.String(), "issues")
}

// falQueue answers status COMPLETED and then the given result answer.
func falQueue(t *testing.T, status int, result string) *fal.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(result))
	}))
	t.Cleanup(server.Close)
	return &fal.Client{HTTP: server.Client(), BaseURL: server.URL, Key: "fal-native-secret-key"}
}

// Owner decision 2026-10-09, after-acceptance path: fal accepted the request,
// the queue completed and the result GET answers 422 with a named field. The
// task fails as invalid_parameters and is refunded like
// upstream_result_rejected (execution_finished, then platform_failure).
func TestNativePollRejectionNamesFieldsAndRefundsPlatformFailure(t *testing.T) {
	hook := captureLogs(t)
	e, plan, wallet, _ := setup(t)
	_, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	provider := falQueue(t, http.StatusUnprocessableEntity, falVoiceRejection)
	resolve := func(context.Context, *model.NativeTask) (Poller, error) { return provider, nil }
	wallet.commands = nil

	task, err := e.Poll(context.Background(), "req", "g", 1, resolve, nil)
	require.NoError(t, err)
	require.Equal(t, "failed", task.Status)
	require.Equal(t, "invalid_parameters", task.ErrorCode)
	require.Equal(t, "upstream-1", task.UpstreamID)
	actions := walletActions(wallet)
	require.Equal(t, 1, countAction(actions, "execution_finished"))
	require.Equal(t, 1, countAction(actions, "settle:failed/platform_failure"))
	require.Less(t, lastIndex(actions, "execution_finished"), lastIndex(actions, "settle:failed/platform_failure"))
	require.Zero(t, countAction(actions, "reject_attempt"))
	require.Zero(t, countAction(actions, "settle:failed/not_accepted"))

	recorder := httptest.NewRecorder()
	WritePublic(recorder, http.StatusOK, task)
	requireVoiceRejection(t, recorder.Body.Bytes())

	// Recovery replays the same refund.
	wallet.commands = nil
	require.NoError(t, e.SyncBilling(context.Background(), task))
	require.Equal(t, []string{"accept_attempt", "execution_finished", "settle:failed/platform_failure", "get"}, walletActions(wallet))

	entry := logEntry(t, hook, "native task failed at the provider")
	require.Equal(t, "req", entry.Data["task_id"])
	require.Equal(t, 422, entry.Data["provider_status"])
	require.Equal(t, "feature_not_supported@body.voice", entry.Data["provider_reason"])
	require.Equal(t, "invalid_parameters", entry.Data["error_code"])
	requireNoSecretsLogged(t, hook, falTextLeaks...)
}

// Without a named field the after-acceptance rejection keeps its old code.
func TestNativePollRejectionWithoutFieldsStaysUpstreamResultRejected(t *testing.T) {
	e, plan, wallet, _ := setup(t)
	_, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	provider := falQueue(t, http.StatusBadRequest, `{"detail":"Voice not found: NoSuchVoice123"}`)
	task, err := e.Poll(context.Background(), "req", "g", 1, func(context.Context, *model.NativeTask) (Poller, error) { return provider, nil }, nil)
	require.NoError(t, err)
	require.Equal(t, "upstream_result_rejected", task.ErrorCode)
	require.Empty(t, task.PublicError)
	require.True(t, refundedWith(wallet, "platform_failure"))
}

// A poll result storage refuses (invalid_parameters without valid issues)
// never fails the task: it stays accepted for the next poll.
func TestNativePollRefusesInvalidParametersWithoutIssues(t *testing.T) {
	e, plan, wallet, _ := setup(t)
	_, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	poll := &pollStub{result: nativeresult.PollResult{Status: "failed", ErrorCode: "invalid_parameters"}}
	_, err = e.Poll(context.Background(), "req", "g", 1, func(context.Context, *model.NativeTask) (Poller, error) { return poll, nil }, nil)
	require.ErrorIs(t, err, model.ErrNativeTaskConflict)
	task, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "queued", task.Status)
	require.False(t, refundedWith(wallet, "platform_failure"))
}

// fixedControlContract freezes the platform control enable_safety_checker:
// the published schema hides it and ValidateRequest refuses it from customers.
const fixedControlContract = `{"version":1,"model":"brand/model/native","input_schema":{"type":"object","required":["seed"],"properties":{"seed":{"type":"integer"}},"additionalProperties":false},"upstream_input_schema":{"type":"object","properties":{"seed":{"type":"integer"},"enable_safety_checker":{"const":true}},"required":["seed","enable_safety_checker"],"additionalProperties":false},"fixed_parameters":{"enable_safety_checker":true},"output_schema":{}}`

const falControlRejection = `{"loc":["body","enable_safety_checker"],"msg":"Safety checker cannot be enabled","type":"feature_not_supported"}`
const falSeedRejection = `{"loc":["body","seed"],"msg":"Input should be less than or equal to 4294967295","type":"less_than_equal"}`

// A rejected registry-frozen platform control is the platform's fault: the
// customer can neither send nor see it, so it is never one of their issues.
// Without another named field the task keeps the generic code; billing is
// unchanged on both paths.
func TestNativeRejectedPlatformControlIsNeverACustomerIssue(t *testing.T) {
	cases := []struct {
		name, detail string
		issues       []nativeresult.ParameterIssue
	}{
		{name: "control only", detail: `{"detail":[` + falControlRejection + `]}`},
		{name: "control and seed", detail: `{"detail":[` + falControlRejection + `,` + falSeedRejection + `]}`,
			issues: []nativeresult.ParameterIssue{{Field: "seed", Rule: "range"}}},
	}
	requirePublic := func(t *testing.T, task *model.NativeTask, code string, issues []nativeresult.ParameterIssue) {
		t.Helper()
		require.Equal(t, "failed", task.Status)
		require.Equal(t, code, task.ErrorCode)
		recorder := httptest.NewRecorder()
		WritePublic(recorder, http.StatusOK, task)
		var got publicFailure
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
		require.Equal(t, code, got.Error.Code)
		require.Equal(t, issues, got.Error.Issues)
		require.NotContains(t, recorder.Body.String(), "enable_safety_checker")
		require.NotContains(t, task.PublicError, "enable_safety_checker")
	}
	for _, tc := range cases {
		t.Run("submit/"+tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(tc.detail))
			}))
			defer server.Close()
			e, plan, wallet, _ := setup(t)
			plan.Contract = json.RawMessage(fixedControlContract)
			e.Provider = &fal.Client{HTTP: server.Client(), BaseURL: server.URL, Key: "fal-native-secret-key"}
			task, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
			require.NoError(t, err)
			code := "upstream_rejected"
			if tc.issues != nil {
				code = "invalid_parameters"
			}
			requirePublic(t, task, code, tc.issues)
			require.True(t, refundedWith(wallet, "not_accepted"))
		})
		t.Run("poll/"+tc.name, func(t *testing.T) {
			hook := captureLogs(t)
			e, plan, wallet, _ := setup(t)
			plan.Contract = json.RawMessage(fixedControlContract)
			_, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
			require.NoError(t, err)
			provider := falQueue(t, http.StatusUnprocessableEntity, tc.detail)
			task, err := e.Poll(context.Background(), "req", "g", 1, func(context.Context, *model.NativeTask) (Poller, error) { return provider, nil }, nil)
			require.NoError(t, err)
			code := "upstream_result_rejected"
			if tc.issues != nil {
				code = "invalid_parameters"
			}
			requirePublic(t, task, code, tc.issues)
			require.True(t, refundedWith(wallet, "platform_failure"))
			// Operators still see the rejected control in the sanitized reason.
			entry := logEntry(t, hook, "native task failed at the provider")
			require.Equal(t, code, entry.Data["error_code"])
			require.Contains(t, entry.Data["provider_reason"], "feature_not_supported@body.enable_safety_checker")
		})
	}
}
