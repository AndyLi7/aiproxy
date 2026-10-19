//nolint:testpackage // White-box coverage uses package-private key.
package balance

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// Own task queue (spec 2026-10-15 §4.1): the queue key is sent only when set,
// because the application's admit schema is strict.
func TestQueueKeyIsSentOnlyWhenSet(t *testing.T) {
	bodies := []string{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		_, _ = w.Write([]byte(`{"code":0,"data":{"id":"op-1","status":"pending"}}`))
	}))
	defer server.Close()

	client := &ExternalHTTP{url: server.URL, key: "private-test-key"}
	for _, queue := range []bool{false, true} {
		_, err := client.Prepayment(
			context.Background(),
			PrepaymentCommand{
				Action:             "admit",
				Group:              "user",
				BillingOperationID: "op-1",
				Queue:              queue,
			},
		)
		require.NoError(t, err)
	}

	require.NotContains(t, bodies[0], "queue")
	require.Contains(t, bodies[1], `"queue":true`)
}

// The wallet's 429 tells a full queue from the active limit and reports the
// account's active limit; both still match ErrPrepaymentTooManyActiveTasks.
func TestTaskLimitRefusalsCarryTheirKindAndLimit(t *testing.T) {
	for _, tc := range []struct {
		body    string
		waiting bool
		limit   int
	}{
		{`{"code":-1,"message":"Too many active tasks","data":{"activeLimit":5}}`, false, 5},
		{`{"code":-1,"message":"Too many waiting tasks","data":{"activeLimit":5}}`, true, 5},
		// An application before the queue answers without data.
		{`{"code":-1,"message":"Too many active tasks"}`, false, 0},
		{`not json`, false, 0},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(tc.body))
		}))
		client := &ExternalHTTP{url: server.URL, key: "private-test-key"}
		_, err := client.Prepayment(
			context.Background(),
			PrepaymentCommand{Action: "begin_attempt", Group: "user", BillingOperationID: "op-1"},
		)

		server.Close()
		require.ErrorIs(t, err, ErrPrepaymentTooManyActiveTasks, tc.body)

		var refusal *TaskLimitError
		require.True(t, errors.As(err, &refusal))
		require.Equal(t, tc.waiting, refusal.Waiting, tc.body)
		require.Equal(t, tc.limit, refusal.ActiveLimit, tc.body)
	}
}

// An application refusal (its envelope, non-zero code) is definite and never
// retried; it is told apart from an outcome the gateway cannot know.
func TestApplicationRefusalIsDefinite(t *testing.T) {
	calls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"code":-1,"message":"Prepayment unavailable"}`))
	}))
	defer server.Close()

	client := &ExternalHTTP{url: server.URL, key: "private-test-key"}
	_, err := client.Prepayment(
		WithSubmitRetry(context.Background()),
		PrepaymentCommand{Action: "begin_attempt", Group: "user", BillingOperationID: "op-1"},
	)
	require.ErrorIs(t, err, ErrPrepaymentRejected)
	require.Equal(t, 1, calls)

	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer edge.Close()

	_, err = (&ExternalHTTP{url: edge.URL, key: "private-test-key"}).Prepayment(
		context.Background(),
		PrepaymentCommand{Action: "begin_attempt", Group: "user", BillingOperationID: "op-1"},
	)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrPrepaymentRejected)
}

func TestReceiptAttemptCount(t *testing.T) {
	for raw, want := range map[string]int{`"[]"`: 0, `"[{\"id\":\"a\",\"state\":\"unknown\"}]"`: 1, `[]`: 0} {
		count, err := PrepaymentReceipt{AttemptsJSON: []byte(raw)}.AttemptCount()
		require.NoError(t, err)
		require.Equal(t, want, count)
	}

	for _, raw := range []string{``, `null`, `"not json"`, `{"x":1}`} {
		_, err := PrepaymentReceipt{AttemptsJSON: []byte(raw)}.AttemptCount()
		require.ErrorIs(t, err, ErrUnreadableAttempts, raw)
	}
}
