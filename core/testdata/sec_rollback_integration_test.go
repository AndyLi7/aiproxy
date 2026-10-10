package balance

import (
	"context"
	"encoding/json"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestSECOldGatewayNewWallet(t *testing.T) {
	u := os.Getenv("SEC_TEST_WALLET_URL")
	require.True(t, strings.HasPrefix(u, "http://127.0.0.1:"))
	backend := NewExternalHTTP(u, "local-sec-test-only")
	for _, id := range []string{"sec-old-binary-one", "sec-old-binary-two"} {
		ctx := ContextWithPricing(context.WithValue(t.Context(), CtxRequestID, id), "USD", "sec-test")
		_, c, e := backend.GetGroupRemainBalance(ctx, model.GroupCache{ID: "u_user_1"})
		require.NoError(t, e)
		for range 2 {
			amount, e := c.PostGroupConsume(ctx, "sec-test", 0.6)
			require.NoError(t, e)
			require.Equal(t, 0.6, amount)
		}
	}
	req, e := http.NewRequest("GET", u+"/_test/ledger", nil)
	require.NoError(t, e)
	req.Header.Set("Authorization", "Bearer local-sec-test-only")
	resp, e := http.DefaultClient.Do(req)
	require.NoError(t, e)
	defer resp.Body.Close()
	var rows []struct {
		Trace  string `json:"source_request_id"`
		Amount int64  `json:"amount_micros"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rows))
	var n int
	var total int64
	for _, r := range rows {
		if strings.HasPrefix(r.Trace, "sec-old-binary-") {
			n++
			total += r.Amount
		}
	}
	require.Equal(t, 2, n)
	require.EqualValues(t, -1200000, total)
}
