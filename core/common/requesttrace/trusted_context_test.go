package requesttrace

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type contextNonceSink struct {
	calls   int
	expires time.Time
	err     error
}

func (s *contextNonceSink) ClaimTraceNonce(_ context.Context, digest string, expires time.Time) (bool, error) {
	s.calls++
	s.expires = expires
	return len(digest) == 64, s.err
}

func TestTrustedContextRoundTripBindsAuthenticatedRequest(t *testing.T) {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	now := time.Unix(1800000000, 0).UTC()
	claims := TrustedClaims{Version: 1, KeyID: "platform-1", TraceID: "11111111111111111111111111111111", ParentSpanID: "22222222222222222222222222222222", GroupID: "account-a", RequestID: "request-a", Source: "playground", Method: "POST", Path: "/v1/images/generations", IssuedAt: now.Unix(), Nonce: "33333333333333333333333333333333"}
	wire, err := SignTrustedContext(claims, key)
	require.NoError(t, err)
	sink := &contextNonceSink{}
	verified, err := VerifyTrustedContext(context.Background(), wire, map[string][]byte{"platform-1": key}, TrustedRequest{GroupID: "account-a", RequestID: "request-a", Method: "POST", Path: "/v1/images/generations"}, now, sink)
	require.NoError(t, err)
	require.Equal(t, "11111111111111111111111111111111", verified.TraceID())
	require.Equal(t, "22222222222222222222222222222222", verified.ParentSpanID())
	require.Equal(t, "playground", verified.Source())
	require.Equal(t, 1, sink.calls)
	require.Equal(t, now.Add(121*time.Second), sink.expires)
}

func TestTrustedContextStorageUnavailableReturnsNoContext(t *testing.T) {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	now := time.Unix(1800000000, 0).UTC()
	wire, err := SignTrustedContext(TrustedClaims{Version: 1, KeyID: "platform-1", TraceID: "11111111111111111111111111111111", ParentSpanID: "22222222222222222222222222222222", GroupID: "account-a", RequestID: "request-a", Source: "admin_demo", Method: "POST", Path: "/v1/videos", IssuedAt: now.Unix(), Nonce: "33333333333333333333333333333333"}, key)
	require.NoError(t, err)
	verified, err := VerifyTrustedContext(context.Background(), wire, map[string][]byte{"platform-1": key}, TrustedRequest{GroupID: "account-a", RequestID: "request-a", Method: "POST", Path: "/v1/videos"}, now, &contextNonceSink{err: errors.New("storage unavailable")})
	require.Error(t, err)
	require.Empty(t, verified.TraceID())
}
