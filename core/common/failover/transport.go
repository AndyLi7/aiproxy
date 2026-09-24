package failover

import (
	"context"
	"errors"
	"net"
	"net/http/httptrace"
	"sync/atomic"
)

// TraceTransport only certifies DNS/dial failures observed before ANY connection
// was acquired. Redirects and adapters making multiple calls remain conservative.
func TraceTransport(ctx context.Context) (context.Context, func(error) Failure) {
	var started, connected, tlsStarted, wroteRequest atomic.Bool
	trace := &httptrace.ClientTrace{
		DNSStart:          func(httptrace.DNSStartInfo) { started.Store(true) },
		ConnectStart:      func(string, string) { started.Store(true) },
		GotConn:           func(httptrace.GotConnInfo) { connected.Store(true) },
		WroteHeaders:      func() { connected.Store(true) },
		WroteRequest:      func(httptrace.WroteRequestInfo) { connected.Store(true); wroteRequest.Store(true) },
		TLSHandshakeStart: func() { tlsStarted.Store(true) },
	}
	return httptrace.WithClientTrace(ctx, trace), func(err error) Failure {
		f := FromError(err)
		if f.Acceptance != Unknown {
			return f
		}
		// Keep acceptance conservative; diagnostic labels must not authorize a
		// retry. Never persist the raw error, which may contain credential URLs.
		if f.Evidence == "" {
			switch {
			case wroteRequest.Load():
				f.Evidence = "transport_error_after_request_write_attempt"
			case connected.Load():
				f.Evidence = "transport_error_after_connection"
			case tlsStarted.Load():
				f.Evidence = "transport_error_during_tls_handshake"
			case started.Load():
				f.Evidence = "transport_error_during_connection_setup"
			default:
				f.Evidence = "transport_error_without_trace"
			}
		}
		if !started.Load() || connected.Load() {
			return f
		}
		var dns *net.DNSError
		var op *net.OpError
		if errors.As(err, &dns) || (errors.As(err, &op) && op.Op == "dial") {
			return Failure{Acceptance: NotAccepted, Class: Transient, Evidence: "transport_failed_before_connection"}
		}
		return f
	}
}
