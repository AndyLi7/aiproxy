package nativetask

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

type insufficientWallet struct{ walletStub }

func (w *insufficientWallet) Prepayment(ctx context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	if c.Action == "admit" {
		return balance.PrepaymentReceipt{}, balance.ErrPrepaymentInsufficientBalance
	}
	return w.walletStub.Prepayment(ctx, c)
}

// refusingWallet refuses admission the way the app wallet does before any provider call.
type refusingWallet struct {
	walletStub
	err error
}

func (w *refusingWallet) Prepayment(ctx context.Context, c balance.PrepaymentCommand) (balance.PrepaymentReceipt, error) {
	if c.Action == "admit" {
		return balance.PrepaymentReceipt{}, w.err
	}
	return w.walletStub.Prepayment(ctx, c)
}

type nativeErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Bare codes were read by generated clients as model problems. Every native
// error now carries a code, a message and a type, and is reported to the log
// hook with the same code.
func TestNativeErrorsCarryCodeMessageAndType(t *testing.T) {
	e, plan, _, p := setup(t)
	type logged struct {
		status        int
		code, message string
	}
	var seen []logged
	h := &HTTP{
		Engine:   e,
		Identity: func(*http.Request) (string, int, error) { return "g", 1, nil },
		OnError:  func(status int, code, message string) { seen = append(seen, logged{status, code, message}) },
	}

	for _, tc := range []struct {
		name    string
		do      func(*httptest.ResponseRecorder)
		status  int
		code    string
		kind    string
		message string
	}{
		{"invalid request id", func(w *httptest.ResponseRecorder) {
			r := request(body)
			r.Header.Set("X-Request-Id", "bad id")
			h.Create(w, r)
		}, 400, "invalid_request_id", "invalid_request_error", errorMessages["invalid_request_id"]},
		{"not a native model", func(w *httptest.ResponseRecorder) {
			h.ResolvePlan = func(*http.Request, []byte) (Plan, Provider, error) { return Plan{}, nil, ErrNotNativeModel }
			h.Create(w, request(body))
		}, 400, "native_model_unavailable", "invalid_request_error", errorMessages["native_model_unavailable"]},
		{"route unavailable", func(w *httptest.ResponseRecorder) {
			h.ResolvePlan = func(*http.Request, []byte) (Plan, Provider, error) { return Plan{}, nil, ErrUnavailable }
			h.Create(w, request(body))
		}, 503, "model_route_unavailable", "api_error", "This model is temporarily unavailable. Retry later with the same X-Request-Id."},
		{"insufficient balance", func(w *httptest.ResponseRecorder) {
			h.Engine = &Engine{DB: e.DB, Wallet: &insufficientWallet{}}
			h.ResolvePlan = func(*http.Request, []byte) (Plan, Provider, error) { return plan, p, nil }
			h.Create(w, request(body))
			h.Engine = e
		}, 402, "insufficient_balance", "insufficient_quota", "Your balance does not cover this request's hold; see pricing.prepayment in /v1/models."},
		{"too many active tasks", func(w *httptest.ResponseRecorder) {
			h.Engine = &Engine{DB: e.DB, Wallet: &refusingWallet{err: balance.ErrPrepaymentTooManyActiveTasks}}
			h.ResolvePlan = func(*http.Request, []byte) (Plan, Provider, error) { return plan, p, nil }
			h.Create(w, request(body))
			h.Engine = e
		}, 429, "too_many_active_tasks", "api_error", "Too many of your tasks are generating at once. Wait for one to finish, then retry with the same X-Request-Id."},
		{"model paused for billing review", func(w *httptest.ResponseRecorder) {
			h.Engine = &Engine{DB: e.DB, Wallet: &refusingWallet{err: balance.ErrPrepaymentModelPaused}}
			h.ResolvePlan = func(*http.Request, []byte) (Plan, Provider, error) { return plan, p, nil }
			h.Create(w, request(body))
			h.Engine = e
		}, 503, "model_unavailable", "api_error", "This model is temporarily unavailable. Retry later with the same X-Request-Id."},
		{"unknown task", func(w *httptest.ResponseRecorder) {
			h.Get(w, httptest.NewRequest(http.MethodGet, "/v1/model-tasks/missing", nil), "missing")
		}, 404, "task_not_found", "not_found_error", "No task with this ID exists for this API key. If your POST may not have reached us (timeout or network error), resend the exact same request body with the same X-Request-Id; it is idempotent and is not charged twice. Do not keep polling an ID that returns 404."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen = nil
			w := httptest.NewRecorder()
			tc.do(w)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			var response nativeErrorBody
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			require.Equal(t, tc.code, response.Error.Code)
			require.Equal(t, tc.kind, response.Error.Type)
			require.Equal(t, tc.message, response.Error.Message)
			require.Equal(t, []logged{{tc.status, tc.code, tc.message}}, seen)
		})
	}
	require.Zero(t, p.calls, "no rejected request may reach the provider")
}

func TestFailedNativeTaskExplainsItsCode(t *testing.T) {
	for code, message := range map[string]string{
		"upstream_task_failed":     "The provider could not generate this result. You were not charged; submit a new task with a new X-Request-Id.",
		"upstream_result_rejected": "The provider returned a result we could not deliver. You were not charged; submit a new task with a new X-Request-Id.",
		"upstream_rejected":        "The provider rejected this request before generating. You were not charged.",
		"invalid_parameters":       "The provider rejected the listed input parameters. You were not charged; fix them and submit a new task with a new X-Request-Id.",
		"submission_timeout":       "The provider did not confirm this task in time. You were not charged; submit a new task with a new X-Request-Id.",
		"something_new":            "This task failed. Contact support with the task ID.",
	} {
		w := httptest.NewRecorder()
		WritePublic(w, http.StatusOK, &model.NativeTask{ID: "t", Model: "m", Status: "failed", ErrorCode: code})
		var response struct {
			Status string            `json:"status"`
			Error  map[string]string `json:"error"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		require.Equal(t, "failed", response.Status)
		require.Equal(t, map[string]string{"code": code, "message": message}, response.Error)
	}
}

// Issues appear only for a failed invalid_parameters task with valid stored
// issues; a stored value that does not validate is never shown.
func TestFailedNativeTaskListsOnlyValidIssues(t *testing.T) {
	stored := `{"issues":[{"field":"voice_setting.voice_id","rule":"unsupported_value"},{"field":"image_urls[0]","rule":"file_size"}]}`
	for _, tc := range []struct {
		name, code, status, publicError, want string
	}{
		{"issues", "invalid_parameters", "failed", stored, `{"code":"invalid_parameters","message":"The provider rejected the listed input parameters. You were not charged; fix them and submit a new task with a new X-Request-Id.","issues":[{"field":"voice_setting.voice_id","rule":"unsupported_value"},{"field":"image_urls[0]","rule":"file_size"}]}`},
		{"other code", "upstream_result_rejected", "failed", stored, `{"code":"upstream_result_rejected","message":"The provider returned a result we could not deliver. You were not charged; submit a new task with a new X-Request-Id."}`},
		{"unreadable", "invalid_parameters", "failed", `{"issues":[{"field":"https://fal.ai","rule":"Voice not found"}]}`, `{"code":"invalid_parameters","message":"The provider rejected the listed input parameters. You were not charged; fix them and submit a new task with a new X-Request-Id."}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			WritePublic(w, http.StatusOK, &model.NativeTask{ID: "t", Model: "m", Status: tc.status, ErrorCode: tc.code, PublicError: tc.publicError})
			var response struct {
				Error json.RawMessage `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			require.JSONEq(t, tc.want, string(response.Error))
		})
	}
	w := httptest.NewRecorder()
	WritePublic(w, http.StatusOK, &model.NativeTask{ID: "t", Model: "m", Status: "running", ErrorCode: "invalid_parameters", PublicError: stored})
	require.NotContains(t, w.Body.String(), "issues")
}
