package nativetask

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
)

// captureLogs records standard-logger entries for one test.
func captureLogs(t *testing.T) *logtest.Hook {
	t.Helper()
	hook := new(logtest.Hook)
	old := logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks))
	logrus.AddHook(hook)
	t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(old) })
	return hook
}

func logEntry(t *testing.T, hook *logtest.Hook, message string) *logrus.Entry {
	t.Helper()
	for _, entry := range hook.AllEntries() {
		if entry.Message == message {
			return entry
		}
	}
	t.Fatalf("no log entry %q", message)
	return nil
}

// requireNoSecretsLogged checks every captured entry, message and fields.
func requireNoSecretsLogged(t *testing.T, hook *logtest.Hook, secrets ...string) {
	t.Helper()
	for _, entry := range hook.AllEntries() {
		text := entry.Message + fmt.Sprint(entry.Data)
		for _, secret := range secrets {
			require.NotContains(t, text, secret)
		}
	}
}

// Owner rule 2026-10-08: 401/402/403/404/429 prove fal created no request. The
// task fails at once with upstream_unavailable and the hold is refunded in full
// through the not-accepted path; the provider is never called again.
func TestNativeProviderUnavailableFailsAndRefundsAtOnce(t *testing.T) {
	for _, status := range []int{401, 402, 403, 404, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			e, plan, w, p := setup(t)
			p.err = adaptor.NewProviderUnavailable(status, "User is locked")
			task, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
			require.NoError(t, err)
			require.Equal(t, "failed", task.Status)
			require.Equal(t, model.UpstreamUnavailableCode, task.ErrorCode)
			require.Equal(t, []string{"admit", "begin_attempt", "reject_attempt", "settle:failed/not_accepted"}, walletActions(w))
			stored, err := model.GetNativeTask(e.DB, "req", "g", 1)
			require.NoError(t, err)
			require.Equal(t, model.UpstreamUnavailableCode, stored.ErrorCode)
			require.Empty(t, stored.UpstreamID)
			// A replay of the same request never calls the provider again.
			_, err = e.Submit(context.Background(), "req", "g", 1, body, plan)
			require.NoError(t, err)
			require.Equal(t, 1, p.calls)
		})
	}
}

// A crash between the durable failure and the wallet refund is finished by any
// later sync with the same identities.
func TestNativeProviderUnavailableRefundIsReplayed(t *testing.T) {
	e, plan, w, p := setup(t)
	p.err = adaptor.NewProviderUnavailable(403, "User is locked")
	w.fail = "reject_attempt"
	task, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.Error(t, err)
	require.Equal(t, "failed", task.Status)
	w.fail = ""
	w.commands = nil
	stored, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.NoError(t, e.SyncBilling(context.Background(), stored))
	require.Equal(t, []string{"reject_attempt", "settle:failed/not_accepted", "get"}, walletActions(w))
	require.Equal(t, 1, p.calls)
}

// End to end through the real fal client: the incident's 403 answer becomes a
// failed task the customer can act on, and operators get one log line with
// the status and fal's reason, never the key or the prompt.
func TestNativeCreateExhaustedFalBalance(t *testing.T) {
	hook := captureLogs(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"detail":"User is locked. Reason: Exhausted balance. Top up your balance at fal.ai/dashboard/billing."}`))
	}))
	defer server.Close()
	e, plan, wallet, _ := setup(t)
	provider := &fal.Client{HTTP: server.Client(), BaseURL: server.URL, Key: "fal-native-secret-key"}
	h := &HTTP{Engine: e, Identity: func(*http.Request) (string, int, error) { return "g", 1, nil },
		ResolvePlan: func(*http.Request, []byte) (Plan, Provider, error) { return plan, provider, nil }}

	recorder := httptest.NewRecorder()
	h.Create(recorder, request(body))
	require.Equal(t, http.StatusAccepted, recorder.Code)
	var got struct {
		Status string `json:"status"`
		Error  struct{ Code, Message string }
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
	require.Equal(t, "failed", got.Status)
	require.Equal(t, "upstream_unavailable", got.Error.Code)
	require.Equal(t, "The provider is temporarily unavailable, so this task was not started. You were not charged; try again later with a new X-Request-Id.", got.Error.Message)
	require.Equal(t, 1, calls)
	require.Equal(t, 1, countAction(walletActions(wallet), "settle:failed/not_accepted"))

	entry := logEntry(t, hook, "native task provider submission failed")
	require.Equal(t, "native", entry.Data["lane"])
	require.Equal(t, "req", entry.Data["task_id"])
	require.Equal(t, "fal-ai/vector/model", entry.Data["endpoint"])
	require.Equal(t, 403, entry.Data["provider_status"])
	require.Contains(t, entry.Data["provider_reason"], "Exhausted balance")
	require.Equal(t, "not_accepted", entry.Data["acceptance"])
	require.Equal(t, "upstream_unavailable", entry.Data["error_code"])
	requireNoSecretsLogged(t, hook, "fal-native-secret-key", "9007199254740993")
}

// Input rejections keep their meaning: the customer's input was refused.
func TestNativeInputRejectionStaysUpstreamRejected(t *testing.T) {
	e, plan, w, p := setup(t)
	p.err = adaptor.NewSubmissionRejected(422, "string_too_long@body.prompt", nil)
	task, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.Equal(t, "upstream_rejected", task.ErrorCode)
	require.Equal(t, "settle:failed/not_accepted", walletActions(w)[3])
}

// An uncertain answer still answers 202 queued (the task is reconciled, never
// resubmitted), but it is no longer silent: the provider failure and the
// error Create absorbed are both logged.
func TestNativeCreateLogsUncertainSubmission(t *testing.T) {
	hook := captureLogs(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("upstream overloaded ", 40)))
	}))
	defer server.Close()
	e, plan, _, _ := setup(t)
	provider := &fal.Client{HTTP: server.Client(), BaseURL: server.URL, Key: "fal-native-secret-key"}
	h := &HTTP{Engine: e, Identity: func(*http.Request) (string, int, error) { return "g", 1, nil },
		ResolvePlan: func(*http.Request, []byte) (Plan, Provider, error) { return plan, provider, nil }}
	recorder := httptest.NewRecorder()
	h.Create(recorder, request(body))
	require.Equal(t, http.StatusAccepted, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"status":"queued"`)

	failed := logEntry(t, hook, "native task provider submission failed")
	require.Equal(t, 502, failed.Data["provider_status"])
	require.Equal(t, "unknown", failed.Data["acceptance"])
	require.Equal(t, "submission_unknown", failed.Data["task_status"])
	require.LessOrEqual(t, len(failed.Data["provider_reason"].(string)), 203)

	absorbed := logEntry(t, hook, "native task create returned stored state after an error")
	require.Equal(t, "req", absorbed.Data["task_id"])
	require.Equal(t, "submission_unknown", absorbed.Data["task_status"])
	require.Equal(t, ErrPending.Error(), absorbed.Data["error"])
	requireNoSecretsLogged(t, hook, "fal-native-secret-key", "9007199254740993")
}
