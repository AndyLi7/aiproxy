package requesttrace

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

const trustedContextDomain = "aiproxy-request-trace-v1."
const maxTrustedContextBytes = 2048

var errTrustedContext = errors.New("requesttrace: trusted context unavailable")

// TrustedClaims is a service-to-service envelope, never a persisted span attribute.
// Sign with an independent service key of at least 32 random bytes.
type TrustedClaims struct {
	Version      int    `json:"v"`
	KeyID        string `json:"kid"`
	TraceID      string `json:"trace_id"`
	ParentSpanID string `json:"parent_span_id"`
	GroupID      string `json:"group_id"`
	RequestID    string `json:"request_id"`
	Source       string `json:"source"`
	Method       string `json:"method"`
	Path         string `json:"path"`
	IssuedAt     int64  `json:"iat"`
	Nonce        string `json:"nonce"`
}

// TrustedRequest must be constructed from authentication and routing results.
type TrustedRequest struct{ GroupID, RequestID, Method, Path string }

// TraceNonceConsumer atomically records a nonce digest in shared storage.
// It must retain the digest until expires and return false if already present.
type TraceNonceConsumer interface {
	ClaimTraceNonce(context.Context, string, time.Time) (bool, error)
}

// VerifiedContext cannot be populated outside this package from raw headers.
type VerifiedContext struct{ claims TrustedClaims }

func (v VerifiedContext) TraceID() string      { return v.claims.TraceID }
func (v VerifiedContext) ParentSpanID() string { return v.claims.ParentSpanID }
func (v VerifiedContext) Source() string       { return v.claims.Source }

func SignTrustedContext(c TrustedClaims, key []byte) (string, error) {
	if len(key) < 32 || !validTrustedClaims(c) {
		return "", errTrustedContext
	}
	data, err := json.Marshal(c)
	if err != nil {
		return "", errTrustedContext
	}
	payload := base64.RawURLEncoding.EncodeToString(data)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(trustedContextDomain + payload))
	wire := payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(wire) > maxTrustedContextBytes {
		return "", errTrustedContext
	}
	return wire, nil
}

// VerifyTrustedContext only returns context after authentication-bound verification
// and shared-store nonce consumption. Callers fall back to local tracing on error.
// No underlying storage error or envelope value is included in returned errors.
func VerifyTrustedContext(ctx context.Context, wire string, keys map[string][]byte, actual TrustedRequest, now time.Time, nonces TraceNonceConsumer) (VerifiedContext, error) {
	if ctx == nil || ctx.Err() != nil || nonces == nil || len(wire) == 0 || len(wire) > maxTrustedContextBytes {
		return VerifiedContext{}, errTrustedContext
	}
	payload, signature, ok := strings.Cut(wire, ".")
	if !ok {
		return VerifiedContext{}, errTrustedContext
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(payload)
	if err != nil {
		return VerifiedContext{}, errTrustedContext
	}
	var c TrustedClaims
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c) != nil || decoder.Decode(new(any)) != io.EOF || !validTrustedClaims(c) {
		return VerifiedContext{}, errTrustedContext
	}
	// Require our canonical encoding, including field ordering and no duplicate keys.
	canonical, err := json.Marshal(c)
	if err != nil || !bytes.Equal(data, canonical) {
		return VerifiedContext{}, errTrustedContext
	}
	key := keys[c.KeyID]
	if len(key) < 32 {
		return VerifiedContext{}, errTrustedContext
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || len(sig) != sha256.Size {
		return VerifiedContext{}, errTrustedContext
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(trustedContextDomain + payload))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return VerifiedContext{}, errTrustedContext
	}
	if c.GroupID != actual.GroupID || c.RequestID != actual.RequestID || c.Method != actual.Method || c.Path != actual.Path {
		return VerifiedContext{}, errTrustedContext
	}
	if now.IsZero() || c.IssuedAt < now.Unix()-120 || c.IssuedAt > now.Unix()+30 {
		return VerifiedContext{}, errTrustedContext
	}
	digest := sha256.Sum256([]byte(c.KeyID + ":" + c.Nonce))
	claimCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	accepted, err := nonces.ClaimTraceNonce(claimCtx, hex.EncodeToString(digest[:]), time.Unix(c.IssuedAt+121, 0).UTC())
	if err != nil || !accepted || claimCtx.Err() != nil {
		return VerifiedContext{}, errTrustedContext
	}
	return VerifiedContext{claims: c}, nil
}

func validTrustedClaims(c TrustedClaims) bool {
	if c.Version != 1 || c.IssuedAt <= 0 || !validRandomID(c.TraceID) || !validRandomID(c.ParentSpanID) || !validRandomID(c.Nonce) {
		return false
	}
	if c.Source != "playground" && c.Source != "admin_demo" {
		return false
	}
	if validateRequiredString("key", c.KeyID, 64) != nil || validateRequiredString("group", c.GroupID, maxGroupIDBytes) != nil || validateRequiredString("request", c.RequestID, maxRequestIDBytes) != nil {
		return false
	}
	if c.Method != "POST" && c.Method != "GET" && c.Method != "DELETE" {
		return false
	}
	return strings.HasPrefix(c.Path, "/v1/") && !strings.ContainsAny(c.Path, "?#") && validateRequiredString("path", c.Path, 512) == nil
}
