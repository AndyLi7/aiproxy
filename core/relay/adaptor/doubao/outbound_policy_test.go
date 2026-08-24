//nolint:testpackage
package doubao

import (
	"net/http"
	"net/http/httptest"
	"testing"

	coremodel "github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/stretchr/testify/require"
)

func TestFetchVideoTaskAppliesChannelPublicOnlyPolicy(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("private upstream must not receive async polling requests")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	channel := &coremodel.Channel{
		BaseURL:       server.URL,
		Configs:       coremodel.ChannelConfigs{"outbound_policy": "public_only"},
		SkipTLSVerify: true,
	}
	response, err := (&Adaptor{}).fetchVideoTask(
		t.Context(),
		channel,
		&coremodel.AsyncUsageInfo{UpstreamID: "task-1"},
	)

	require.Nil(t, response)
	require.ErrorContains(t, err, "non-public address")
}

func TestFetchVideoContentAppliesChannelPublicOnlyPolicy(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("private upstream must not receive video download requests")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	m := meta.NewMeta(&coremodel.Channel{
		Configs:       coremodel.ChannelConfigs{"outbound_policy": "public_only"},
		SkipTLSVerify: true,
	}, 0, "", coremodel.ModelConfig{})
	response, err := fetchDoubaoVideoContent(t.Context(), m, server.URL)

	require.Nil(t, response)
	require.ErrorContains(t, err, "non-public address")
}
