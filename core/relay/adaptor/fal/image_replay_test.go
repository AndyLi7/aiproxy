package fal_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"github.com/stretchr/testify/require"
)

type imageReplayTransport func(*http.Request) (*http.Response, error)

func (f imageReplayTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFalSubmissionDisablesTransportReplayAndRedirect(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/again")
				w.WriteHeader(status)
			}))
			defer server.Close()
			client := server.Client()
			transport := client.Transport
			inspected := 0
			client.Transport = imageReplayTransport(func(r *http.Request) (*http.Response, error) {
				inspected++
				require.Equal(t, http.MethodPost, r.Method)
				require.Nil(t, r.GetBody)
				r.Header.Set("Idempotency-Key", "test-logical-request")
				return transport.RoundTrip(r)
			})
			_, err := (&fal.Client{HTTP: client, BaseURL: server.URL}).Submit(t.Context(), "fal-ai/test", []byte(`{"prompt":"test"}`))
			require.Error(t, err)
			require.Equal(t, 1, calls)
			require.Equal(t, 1, inspected)
		})
	}
}
