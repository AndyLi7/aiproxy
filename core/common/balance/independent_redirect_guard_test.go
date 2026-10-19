//nolint:testpackage // White-box coverage needs the package-private consumePricing fixture or implementation.
package balance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/labring/aiproxy/core/common/actionguard"
	"github.com/stretchr/testify/require"
)

func TestIndependentAccountingRedirectMustCheckClaim(t *testing.T) {
	for _, provider := range []string{"external", "sealos"} {
		t.Run(provider, func(t *testing.T) {
			var (
				lost         atomic.Bool
				late, checks atomic.Int32
			)

			srv := httptest.NewServer(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/late-debit" {
						lost.Store(true)
						http.Redirect(w, r, "/late-debit", http.StatusTemporaryRedirect)
						return
					}

					late.Add(1)

					if r.Method != http.MethodPost {
						t.Errorf("unexpected method %s", r.Method)
					}

					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"code":0,"data":{"charged":0.3}}`))
				}),
			)
			defer srv.Close()

			ctx := actionguard.With(context.Background(), func(context.Context) error {
				checks.Add(1)

				if lost.Load() {
					return errors.New("owner replaced")
				}

				return nil
			})
			if provider == "external" {
				_, _ = NewExternalHTTP(
					srv.URL,
					"fixture",
				).postConsume(ctx, "group", "token", .3, "original", consumePricing{})
			} else {
				c := newSealosPostGroupConsumer(srv.URL, "group", "uid")
				_ = c.postConsume(ctx, 300000, "token")
			}

			if late.Load() != 0 {
				t.Fatalf(
					"lost owner still sent %d redirected POST; guard calls=%d",
					late.Load(),
					checks.Load(),
				)
			}
		})
	}
}

func TestAccountingRedirectScopeAndOriginalPayload(t *testing.T) {
	for _, provider := range []string{"external", "sealos"} {
		for _, code := range []int{307, 308} {
			for _, scope := range []string{"live", "lost", "legacy"} {
				t.Run(fmt.Sprintf("%s/%d/%s", provider, code, scope), func(t *testing.T) {
					var (
						lost  atomic.Bool
						calls atomic.Int32
						first []byte
					)

					ownerErr := errors.New("original owner lost")

					server := httptest.NewServer(
						http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							calls.Add(1)

							body, err := io.ReadAll(r.Body)
							if err != nil {
								t.Error(err)
							}

							if r.URL.Path != "/finish" {
								first = body

								lost.Store(true)
								http.Redirect(w, r, "/finish", code)

								return
							}

							if r.Method != http.MethodPost || !bytes.Equal(first, body) {
								t.Error("redirect changed the frozen consume payload")
							}

							w.Header().Set("Content-Type", "application/json")
							_, _ = w.Write([]byte(`{"code":0,"data":{"charged":0.3}}`))
						}),
					)
					defer server.Close()

					ctx := ContextWithBillingOperationID(context.Background(), "original-operation")
					if scope != "legacy" {
						ctx = actionguard.With(ctx, func(context.Context) error {
							if scope == "lost" && lost.Load() {
								return ownerErr
							}
							return nil
						})
					}

					var err error
					if provider == "external" {
						_, err = NewExternalHTTP(
							server.URL,
							"fixture",
						).postConsume(ctx, "group", "token", .3, "original-request", consumePricing{})
					} else {
						err = newSealosPostGroupConsumer(
							server.URL,
							"group",
							"uid",
						).postConsume(ctx, 300000, "token")
					}

					if scope == "lost" {
						require.ErrorIs(t, err, ownerErr)
						require.Equal(t, int32(1), calls.Load())
					} else {
						require.NoError(t, err)
						require.Equal(t, int32(2), calls.Load())
					}
				})
			}
		}
	}
}
