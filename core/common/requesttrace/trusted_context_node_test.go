package requesttrace

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"os/exec"
	"testing"
	"time"
)

func TestTrustedContextNodeSignerInteroperates(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node required for cross-language fixture")
	}
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	now := time.Now().UTC()
	claims := TrustedClaims{Version: 1, KeyID: "app-1", TraceID: "11111111111111111111111111111111", ParentSpanID: "22222222222222222222222222222222", GroupID: "group-a", RequestID: "request-a", Source: "playground", Method: "POST", Path: "/v1/videos", IssuedAt: now.Unix(), Nonce: "33333333333333333333333333333333"}
	input, err := json.Marshal(claims)
	require.NoError(t, err)
	// Independent Node JSON serialization and HMAC implementation, no network calls.
	script := `const c=require('crypto'); const x=JSON.parse(process.argv[1]); const p=Buffer.from(JSON.stringify(x)).toString('base64url'); process.stdout.write(p+'.'+c.createHmac('sha256',Buffer.from(process.argv[2],'base64')).update('aiproxy-request-trace-v1.'+p).digest('base64url'));`
	wire, err := exec.Command("node", "-e", script, string(input), base64.StdEncoding.EncodeToString(key)).Output()
	require.NoError(t, err)
	v, err := VerifyTrustedContext(context.Background(), string(wire), map[string][]byte{"app-1": key}, TrustedRequest{GroupID: "group-a", RequestID: "request-a", Method: "POST", Path: "/v1/videos"}, now, &contextNonceSink{})
	require.NoError(t, err)
	require.Equal(t, claims.TraceID, v.TraceID())
}
