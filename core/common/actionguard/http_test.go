package actionguard

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHTTPScopedRedirectPreservesPolicyAndRechecksAfterCallback(t *testing.T) {
	for _, policy := range []string{"lost-in-callback", "reject", "last-response", "default-limit"} {
		t.Run(policy, func(t *testing.T) {
			var calls atomic.Int32
			var lost atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Redirect(w, r, "/next", 307) }))
			defer server.Close()
			ownerErr := errors.New("owner lost")
			policyErr := errors.New("policy denied")
			client := &http.Client{}
			if policy != "default-limit" {
				client.CheckRedirect = func(*http.Request, []*http.Request) error {
					switch policy {
					case "lost-in-callback":
						lost.Store(true)
						return nil
					case "reject":
						return policyErr
					default:
						return http.ErrUseLastResponse
					}
				}
			}
			ctx := With(context.Background(), func(context.Context) error {
				if lost.Load() {
					return ownerErr
				}
				return nil
			})
			req, err := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
			require.NoError(t, err)
			resp, err := DoHTTP(client, req)
			if resp != nil {
				resp.Body.Close()
			}
			switch policy {
			case "lost-in-callback":
				require.ErrorIs(t, err, ownerErr)
			case "reject":
				require.ErrorIs(t, err, policyErr)
			case "last-response":
				require.NoError(t, err)
				require.Equal(t, 307, resp.StatusCode)
			case "default-limit":
				require.ErrorContains(t, err, "stopped after 10 redirects")
			}
			want := int32(1)
			if policy == "default-limit" {
				want = 10
			}
			require.Equal(t, want, calls.Load())
			if policy == "default-limit" {
				require.Nil(t, client.CheckRedirect)
			}
		})
	}
}
