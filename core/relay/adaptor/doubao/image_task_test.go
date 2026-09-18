//nolint:testpackage
package doubao

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/stretchr/testify/require"
)

func syncContract() []byte {
	return []byte(
		`{"provider_contract_version":1,"selected_provider_binding":{"provider":"ark","id":"ark","revision":"1","contractHash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"providers":{"ark":{"upstream":{"id":"ark","revision":"1","contractHash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","endpoint":"seedream-test","outputJsonSchema":{"type":"object","required":["data"],"properties":{"data":{"type":"array","minItems":1}}}}}}}`,
	)
}

func TestSyncImageBridgeOneRequest(t *testing.T) {
	calls := 0

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++

		require.Equal(t, "/api/v3/images/generations", r.URL.Path)
		require.Equal(t, "Bearer secret", r.Header.Get("Authorization"))

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "seedream-test", body["model"])

		_, _ = w.Write([]byte(`{"data":[{"url":"https://example.com/a.png"}]}`))
	}))
	defer s.Close()

	m := &meta.Meta{
		ActualModel: "seedream-test",
		Channel:     meta.ChannelMeta{BaseURL: s.URL + "/api/v3", Key: "secret"},
	}
	got, err := (&Adaptor{}).GenerateImage(
		t.Context(),
		m,
		[]byte(`{"prompt":"hello","stream":false,"response_format":"url"}`),
		syncContract(),
	)
	require.NoError(t, err)
	require.Equal(t, "completed", got.Status)
	require.Len(t, got.Data, 1)
	require.Equal(t, 1, calls)
}

func TestSyncImageBridgeRejectsRedirectAndInvalidResult(t *testing.T) {
	for _, tc := range []struct {
		code             int
		body             string
		failed, rejected bool
	}{{302, "", false, false}, {400, "", false, true}, {200, `{"data":[{"url":"http://example.com/a"}]}`, true, false}, {200, `{"unexpected":[]}`, true, false}} {
		t.Run(strings.Join([]string{http.StatusText(tc.code), tc.body}, "-"), func(t *testing.T) {
			calls := 0

			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++

				w.Header().Set("Location", "/again")
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()

			got, err := (&Adaptor{}).GenerateImage(
				t.Context(),
				&meta.Meta{ActualModel: "seedream-test", Channel: meta.ChannelMeta{BaseURL: s.URL}},
				[]byte(`{"stream":false,"response_format":"url"}`),
				syncContract(),
			)
			require.Equal(t, 1, calls)

			switch {
			case tc.failed:
				require.NoError(t, err)
				require.Equal(t, "failed", got.Status)
			case tc.rejected:
				require.ErrorIs(t, err, adaptor.ErrImageSubmissionRejected)
			default:
				require.Error(t, err)
				require.NotErrorIs(t, err, adaptor.ErrImageSubmissionRejected)
			}
		})
	}
}
