package failover

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"testing"
	"time"
)

func TestTransportEvidence(t *testing.T) {
	for _, tc := range []struct {
		name               string
		started, connected bool
		err                error
		want               Acceptance
	}{
		{"unobserved dial", false, false, &net.OpError{Op: "dial", Err: errors.New("refused")}, Unknown},
		{"observed dial", true, false, &net.OpError{Op: "dial", Err: errors.New("refused")}, NotAccepted},
		{"observed DNS", true, false, &net.DNSError{Err: "not found"}, NotAccepted},
		{"redirect dial", true, true, &net.OpError{Op: "dial", Err: errors.New("refused")}, Unknown},
		{"response timeout", true, true, context.DeadlineExceeded, Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, classify := TraceTransport(context.Background())
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
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, classify := TraceTransport(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address, nil)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("expected refused connection")
	}
	if f := classify(err); f.Acceptance != NotAccepted {
		t.Fatalf("%+v: %v", f, err)
	}
}
