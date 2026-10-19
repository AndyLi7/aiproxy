package failover_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/failover"
)

func TestTransportEvidence(t *testing.T) {
	for _, tc := range []struct {
		name               string
		started, connected bool
		err                error
		want               failover.Acceptance
	}{
		{"unobserved dial", false, false, &net.OpError{Op: "dial", Err: errors.New("refused")}, failover.Unknown},
		{"observed dial", true, false, &net.OpError{Op: "dial", Err: errors.New("refused")}, failover.NotAccepted},
		{"observed DNS", true, false, &net.DNSError{Err: "not found"}, failover.NotAccepted},
		{"redirect dial", true, true, &net.OpError{Op: "dial", Err: errors.New("refused")}, failover.Unknown},
		{"response timeout", true, true, context.DeadlineExceeded, failover.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, classify := failover.TraceTransport(context.Background())

			tr := httptrace.ContextClientTrace(ctx)
			if tc.started {
				tr.ConnectStart("tcp", "test")
			}

			if tc.connected {
				tr.GotConn(httptrace.GotConnInfo{})
			}

			if f := classify(tc.err); f.Acceptance != tc.want {
				t.Fatalf("%+v", f)
			}
		})
	}
}

func TestTransportLocalConnectionRefused(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	address := listener.Addr().String()
	listener.Close()

	ctx, classify := failover.TraceTransport(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address, nil)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}

	response, err := client.Do(req)
	if response != nil {
		response.Body.Close()
	}

	if err == nil {
		t.Fatal("expected refused connection")
	}

	if f := classify(err); f.Acceptance != failover.NotAccepted {
		t.Fatalf("%+v: %v", f, err)
	}
}

func TestTransportDiagnosticsDoNotAuthorizeRetryOrExposeRawError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		observe func(*httptrace.ClientTrace)
		want    string
	}{
		{"tls", func(tr *httptrace.ClientTrace) { tr.TLSHandshakeStart() }, "transport_error_during_tls_handshake"},
		{"connected", func(tr *httptrace.ClientTrace) { tr.GotConn(httptrace.GotConnInfo{}) }, "transport_error_after_connection"},
		{"write", func(tr *httptrace.ClientTrace) {
			tr.WroteRequest(httptrace.WroteRequestInfo{Err: errors.New("secret")})
		}, "transport_error_after_request_write_attempt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, classify := failover.TraceTransport(context.Background())
			tc.observe(httptrace.ContextClientTrace(ctx))

			f := classify(errors.New("https://user:secret@example.com failed"))
			if f.Acceptance != failover.Unknown || f.Evidence != tc.want {
				t.Fatalf("unexpected diagnostic: %+v", f)
			}
		})
	}
}
