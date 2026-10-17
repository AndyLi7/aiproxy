package balance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/actionguard"
	"github.com/stretchr/testify/require"
)

// The hold's submission fields are read on their own and leniently: a value
// of another type reads as unknown and never fails the receipt, so a change
// to one of them cannot stop admissions or refunds.
func TestPrepaymentReceiptReadsWalletSubmissionLeniently(t *testing.T) {
	for _, tc := range []struct {
		name, data         string
		submission         string
		admitted, unknown  bool
		state              string
		present, readError bool
	}{
		{"application today", `"submissionStatus":"unknown","attemptsJson":"[{\"id\":\"op-1:1\",\"routeId\":\"r\",\"state\":\"unknown\"}]","createdAt":"2026-10-13T11:21:00.000Z","unknownSince":"2026-10-13T11:21:04.500Z"`, "unknown", true, true, "unknown", true, false},
		{"no attempt yet", `"submissionStatus":"not_submitted","attemptsJson":"[]","createdAt":"2026-10-13T11:21:00.000Z","unknownSince":null`, "not_submitted", true, false, "", false, false},
		{"attempts as an array", `"submissionStatus":"not_submitted","attemptsJson":[{"id":"op-1:1","state":"rejected"}],"createdAt":"2026-10-13T11:21:00.000Z"`, "not_submitted", true, false, "rejected", true, false},
		{"other types", `"submissionStatus":5,"attemptsJson":{"id":"op-1:1"},"createdAt":12,"unknownSince":true`, "", false, false, "", false, true},
		{"unparsable text", `"attemptsJson":"[{","createdAt":"yesterday"`, "", false, false, "", false, true},
		{"absent", `"usageSource":"api"`, "", false, false, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"code":0,"data":{"id":"op-1","status":"pending","prepaidMicros":200,` + tc.data + `}}`))
			}))
			defer server.Close()
			client := &ExternalHTTP{url: server.URL, key: "private-test-key"}
			for _, action := range []string{"get", "admit", "settle"} {
				receipt, err := client.Prepayment(context.Background(), PrepaymentCommand{Action: action, Group: "user", BillingOperationID: "op-1"})
				require.NoError(t, err, action)
				require.Equal(t, "pending", receipt.Status)
			}
			receipt, err := client.Prepayment(context.Background(), PrepaymentCommand{Action: "get", Group: "user", BillingOperationID: "op-1"})
			require.NoError(t, err)
			// The gateway stores the receipt as JSON and reads it back later.
			stored, err := json.Marshal(receipt)
			require.NoError(t, err)
			var reread PrepaymentReceipt
			require.NoError(t, json.Unmarshal(stored, &reread))
			for _, r := range []PrepaymentReceipt{receipt, reread} {
				require.Equal(t, tc.submission, r.Submission())
				_, ok := r.AdmittedAt()
				require.Equal(t, tc.admitted, ok)
				_, ok = r.UnknownAt()
				require.Equal(t, tc.unknown, ok)
				state, present, err := r.AttemptState("op-1:1")
				require.Equal(t, tc.state, state)
				require.Equal(t, tc.present, present)
				if tc.readError {
					require.ErrorIs(t, err, ErrUnreadableAttempts)
				} else {
					require.NoError(t, err)
				}
			}
		})
	}
}

// roundTrip lets a test answer the wallet's HTTP requests directly.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// scriptedWallet answers each request with the next scripted reply and keeps
// every request body.
type scriptedWallet struct {
	mu      sync.Mutex
	bodies  []string
	replies []func() (*http.Response, error)
}

func reply(status int, body string) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	}
}

func fail(err error) func() (*http.Response, error) {
	return func() (*http.Response, error) { return nil, err }
}

var (
	refused  = &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	timedOut = &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	reset    = &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	edge502  = reply(http.StatusBadGateway, `<html><body>Application failed to respond</body></html>`)
	receipt  = reply(http.StatusOK, `{"code":0,"data":{"id":"op-1","status":"pending","claimed":true}}`)
)

// useScriptedWallet routes the wallet client through script and shrinks the
// retry backoff.
func useScriptedWallet(t *testing.T, replies ...func() (*http.Response, error)) (*ExternalHTTP, *scriptedWallet) {
	t.Helper()
	script := &scriptedWallet{replies: replies}
	oldClient, oldBackoff := externalHTTPClient, prepaymentRetryBackoff
	externalHTTPClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		script.mu.Lock()
		defer script.mu.Unlock()
		script.bodies = append(script.bodies, string(raw))
		require.NotEmpty(t, script.replies, "unexpected wallet call %d", len(script.bodies))
		next := script.replies[0]
		script.replies = script.replies[1:]
		return next()
	})}
	prepaymentRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { externalHTTPClient, prepaymentRetryBackoff = oldClient, oldBackoff })
	return &ExternalHTTP{url: "http://wallet.internal", key: "private-test-key"}, script
}

func submitCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(WithSubmitRetry(context.Background()), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// R1: a submission's admit and begin_attempt are sent again, with the same
// body, when the connection was refused or a 5xx came from the platform edge
// instead of the application. The edge page does not prove the wallet never
// received the command; the retry is safe because both commands replay
// idempotently (a replayed admit returns the existing hold, a replayed
// begin_attempt answers claimed:false).
func TestPrepaymentRetriesIdempotentSubmitCommandsWithSameBody(t *testing.T) {
	for _, action := range []string{"admit", "begin_attempt"} {
		t.Run(action, func(t *testing.T) {
			client, script := useScriptedWallet(t, fail(refused), edge502, receipt)
			got, err := client.Prepayment(submitCtx(t), PrepaymentCommand{Action: action, Group: "user", BillingOperationID: "op-1", AttemptID: "op-1:1", RouteID: "r"})
			require.NoError(t, err)
			require.Equal(t, "op-1", got.ID)
			require.Len(t, script.bodies, 3)
			require.Equal(t, script.bodies[0], script.bodies[1])
			require.Equal(t, script.bodies[0], script.bodies[2])
		})
	}
	t.Run("at most three tries", func(t *testing.T) {
		client, script := useScriptedWallet(t, fail(refused), fail(refused), fail(refused))
		_, err := client.Prepayment(submitCtx(t), PrepaymentCommand{Action: "admit", Group: "user", BillingOperationID: "op-1"})
		require.Error(t, err)
		require.Len(t, script.bodies, 3)
	})
}

// A timeout, a reset connection, an edge 504, every refusal and every
// application answer is sent once: the submission answers it, and a same-ID
// retry replays the command later. Refusals are final.
func TestPrepaymentNeverRetriesTimeoutsOrApplicationAnswers(t *testing.T) {
	for name, answer := range map[string]func() (*http.Response, error){
		"timeout":                 fail(timedOut),
		"connection reset":        fail(reset),
		"edge timeout 504":        reply(http.StatusGatewayTimeout, `<html>Gateway Timeout</html>`),
		"application 500":         reply(http.StatusInternalServerError, `{"code":-1,"message":"boom"}`),
		"application code -1":     reply(http.StatusOK, `{"code":-1,"message":"Prepayment unavailable"}`),
		"insufficient balance":    reply(http.StatusPaymentRequired, `{"code":-1,"message":"Insufficient balance"}`),
		"model paused":            reply(http.StatusLocked, `{"code":-1,"message":"paused"}`),
		"too many active tasks":   reply(http.StatusTooManyRequests, `{"code":-1,"message":"busy"}`),
		"not found":               reply(http.StatusNotFound, `{"code":-1,"message":"Prepayment not found"}`),
		"edge not found":          reply(http.StatusNotFound, `<html>Not Found</html>`),
		"application bad request": reply(http.StatusBadRequest, `bad`),
	} {
		t.Run(name, func(t *testing.T) {
			for _, action := range []string{"admit", "begin_attempt"} {
				client, script := useScriptedWallet(t, answer)
				_, err := client.Prepayment(submitCtx(t), PrepaymentCommand{Action: action, Group: "user", BillingOperationID: "op-1"})
				require.Error(t, err)
				require.Len(t, script.bodies, 1, action)
			}
		})
	}
}

// Retries belong to the submission path only. Recovery and every other
// command retry on the next pass instead, so a wallet outage never slows the
// recovery loops.
func TestPrepaymentRetriesOnlyMarkedSubmitCommands(t *testing.T) {
	client, script := useScriptedWallet(t, fail(refused))
	_, err := client.Prepayment(context.Background(), PrepaymentCommand{Action: "admit", Group: "user", BillingOperationID: "op-1"})
	require.Error(t, err)
	require.Len(t, script.bodies, 1, "an unmarked admit is sent once")
	for _, action := range []string{"get", "accept_attempt", "reject_attempt", "settle", "delivered", "execution_finished"} {
		client, script := useScriptedWallet(t, fail(refused))
		_, err := client.Prepayment(submitCtx(t), PrepaymentCommand{Action: action, Group: "user", BillingOperationID: "op-1"})
		require.Error(t, err)
		require.Len(t, script.bodies, 1, action)
	}
}

// The submission's wallet budget (its context deadline) also bounds retries.
func TestPrepaymentSubmitRetriesStopAtTheBudget(t *testing.T) {
	client, script := useScriptedWallet(t, fail(refused), receipt)
	prepaymentRetryBackoff = []time.Duration{time.Second, time.Second}
	ctx, cancel := context.WithTimeout(WithSubmitRetry(context.Background()), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := client.Prepayment(ctx, PrepaymentCommand{Action: "admit", Group: "user", BillingOperationID: "op-1"})
	require.Error(t, err)
	require.Len(t, script.bodies, 1)
	require.Less(t, time.Since(started), time.Second)
}

// A retry is never sent when the budget left would cut it short of the full
// request timeout: the wallet could process it after the submission gave up
// (incident 2026-10-13), while the same-ID retry replays it safely.
func TestPrepaymentSubmitRetriesNeedTheFullRequestTimeout(t *testing.T) {
	for _, action := range []string{"admit", "begin_attempt"} {
		client, script := useScriptedWallet(t, edge502, receipt)
		ctx, cancel := context.WithTimeout(WithSubmitRetry(context.Background()), externalRequestTimeout-time.Second)
		_, err := client.Prepayment(ctx, PrepaymentCommand{Action: action, Group: "user", BillingOperationID: "op-1", AttemptID: "op-1:1", RouteID: "r"})
		cancel()
		require.Error(t, err, action)
		require.Len(t, script.bodies, 1, action)
	}
	ctx, cancel := context.WithTimeout(context.Background(), externalRequestTimeout+time.Second)
	defer cancel()
	require.True(t, FullRequestLeft(ctx))
	require.True(t, FullRequestLeft(context.Background()), "no deadline")
	short, cancelShort := context.WithTimeout(context.Background(), externalRequestTimeout-time.Millisecond)
	defer cancelShort()
	require.False(t, FullRequestLeft(short))
}

// A refused connection is recognised as undelivered; a connection the server
// accepted and dropped after reading the request is not.
func TestNeverDeliveredOnlyForUnconnectedRequests(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closedURL := "http://" + listener.Addr().String()
	require.NoError(t, listener.Close())
	_, err = http.Post(closedURL, "application/json", strings.NewReader(`{}`))
	require.Error(t, err)
	require.True(t, neverDelivered(err), "%v", err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		conn, _, hijackErr := w.(http.Hijacker).Hijack()
		require.NoError(t, hijackErr)
		_ = conn.Close()
	}))
	defer server.Close()
	_, err = http.Post(server.URL, "application/json", strings.NewReader(`{}`))
	require.Error(t, err)
	require.False(t, neverDelivered(err), "%v", err)
}

// R5: only the application's own "Prepayment not found" envelope confirms
// that the wallet holds nothing. Any 404 still reads as not found, as before.
func TestPrepaymentNotFoundConfirmedOnlyByAppEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		confirmed  bool
	}{
		{"application envelope", `{"code":-1,"message":"Prepayment not found"}`, true},
		{"platform page", `<html>Application not found</html>`, false},
		{"empty", ``, false},
		{"other application message", `{"code":-1,"message":"Not found"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := useScriptedWallet(t, reply(http.StatusNotFound, tc.body))
			_, err := client.Prepayment(context.Background(), PrepaymentCommand{Action: "settle", Group: "user", BillingOperationID: "op-1"})
			require.ErrorIs(t, err, ErrPrepaymentNotFound)
			require.Equal(t, tc.confirmed, WalletConfirmedNotFound(err))
		})
	}
	require.False(t, WalletConfirmedNotFound(ErrPrepaymentNotFound))
	require.False(t, WalletConfirmedNotFound(errors.New("prepayment not found")))
}

func TestPrepaymentRetryRechecksOriginalOwnerBeforeHTTP(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprintf("lost-%v", lost), func(t *testing.T) {
			valid := true
			client, script := useScriptedWallet(t, func() (*http.Response, error) { valid = !lost; return edge502() }, receipt)
			ownerErr := errors.New("original owner lost")
			ctx := actionguard.With(submitCtx(t), func(context.Context) error {
				if !valid {
					return ownerErr
				}
				return nil
			})
			_, err := client.Prepayment(ctx, PrepaymentCommand{Action: "begin_attempt", Group: "user", BillingOperationID: "op-1"})
			if lost {
				require.ErrorIs(t, err, ownerErr)
				require.Len(t, script.bodies, 1)
			} else {
				require.NoError(t, err)
				require.Len(t, script.bodies, 2)
				require.Equal(t, script.bodies[0], script.bodies[1])
			}
		})
	}
}
