package doubao

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/utils"
	"github.com/stretchr/testify/require"
)

type imageReplayTransport func(*http.Request) (*http.Response, error)

func (f imageReplayTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestArkSubmissionDisablesTransportReplayAndRedirect(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/again")
				w.WriteHeader(status)
			}))
			defer server.Close()
			m := &meta.Meta{ActualModel: "seedream-test", Channel: meta.ChannelMeta{BaseURL: server.URL}}
			client, err := utils.LoadHTTPClientWithOutboundPolicyE(45*time.Second, "", false, utils.OutboundPolicyFromConfigs(m.ChannelConfigs))
			require.NoError(t, err)
			transport := client.Transport
			t.Cleanup(func() { client.Transport = transport })
			inspected := 0
			client.Transport = imageReplayTransport(func(r *http.Request) (*http.Response, error) {
				inspected++
				require.Equal(t, http.MethodPost, r.Method)
				require.Nil(t, r.GetBody)
				r.Header.Set("Idempotency-Key", "test-logical-request")
				return transport.RoundTrip(r)
			})
			_, err = (&Adaptor{}).GenerateImage(t.Context(), m, []byte(`{"stream":false,"response_format":"url"}`), syncContract())
			require.Error(t, err)
			require.Equal(t, 1, calls)
			require.Equal(t, 1, inspected)
		})
	}
}
