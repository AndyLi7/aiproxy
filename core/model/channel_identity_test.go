package model_test

import (
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestChannelIdentitySeparatesRoutingAndEveryUpstreamField(t *testing.T) {
	base := model.Channel{
		ID:      1,
		Key:     "synthetic-key",
		Type:    model.ChannelTypeFal,
		BaseURL: "https://example.invalid",
		Configs: model.ChannelConfigs{
			"region": "region-a",
			"nested": map[string]any{"z": 2, "a": 1},
		},
	}
	expected, err := model.ChannelIdentityOf(&base)
	require.NoError(t, err)
	require.Len(t, expected.IdentityHash, 64)
	require.Len(t, expected.KeyDigest, 64)

	routing := base
	routing.Name = "renamed"
	routing.Status = model.ChannelStatusDisabled
	routing.Models = []string{"another"}
	routing.Sets = []string{"private"}
	routing.Configs = model.ChannelConfigs{
		"region":                            "region-a",
		"nested":                            map[string]any{"a": 1, "z": 2},
		"unused":                            nil,
		model.ChannelConfigProviderBindings: map[string]any{"model": "route"},
		model.ChannelConfigOutboundPolicy:   "public_only",
	}
	unchanged, err := model.ChannelIdentityOf(&routing)
	require.NoError(t, err)
	require.Equal(t, expected, unchanged)

	for _, change := range []func(*model.Channel){
		func(c *model.Channel) { c.Key = "synthetic-different" },
		func(c *model.Channel) { c.Type = model.ChannelTypeOpenAI },
		func(c *model.Channel) { c.BaseURL = "https://other.invalid" },
		func(c *model.Channel) { c.ProxyURL = "http://proxy.invalid" },
		func(c *model.Channel) { c.SkipTLSVerify = true },
		func(c *model.Channel) { c.Configs = model.ChannelConfigs{"region": "region-b"} },
	} {
		changed := base
		change(&changed)
		identity, err := model.ChannelIdentityOf(&changed)
		require.NoError(t, err)
		require.NotEqual(t, expected.IdentityHash, identity.IdentityHash)
	}

	broken := base
	broken.Configs = model.ChannelConfigs{"bad": make(chan int)}
	_, err = model.ChannelIdentityOf(&broken)
	require.Error(t, err)
}
