//nolint:testpackage
package utils

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	coremodel "github.com/labring/aiproxy/core/model"
	relaymeta "github.com/labring/aiproxy/core/relay/meta"
	"github.com/stretchr/testify/require"
)

func TestPublicOnlyDialRejectsNonPublicAddresses(t *testing.T) {
	t.Parallel()

	for _, address := range []string{
		"127.0.0.1",
		"10.0.0.1",
		"100.64.0.1",
		"169.254.1.1",
		"172.16.0.1",
		"192.168.1.1",
		"198.18.0.1",
		"::1",
		"fc00::1",
		"fe80::1",
		"::ffff:127.0.0.1",
		"::ffff:0:127.0.0.1",
		"::ffff:1:127.0.0.1",
		"64:ff9b::127.0.0.1",
		"2002:7f00:1::1",
		"2001:db8::1",
		"3fff::1",
		"5f00::1",
	} {
		t.Run(address, func(t *testing.T) {
			t.Parallel()

			dialCalled := false
			dial := publicOnlyDialContext(
				func(context.Context, string, string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr(address)}, nil
				},
				func(context.Context, string, string) (net.Conn, error) {
					dialCalled = true
					return nil, errors.New("unexpected dial")
				},
			)

			conn, err := dial(t.Context(), "tcp", "provider.example:443")

			require.Nil(t, conn)
			require.ErrorContains(t, err, "non-public address")
			require.False(t, dialCalled)
		})
	}
}

func TestPublicOnlyDialRejectsIPv4MappedPublicAddress(t *testing.T) {
	t.Parallel()

	dialCalled := false
	dial := publicOnlyDialContext(
		func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("::ffff:93.184.216.34")}, nil
		},
		func(context.Context, string, string) (net.Conn, error) {
			dialCalled = true
			return nil, errors.New("unexpected dial")
		},
	)

	conn, err := dial(t.Context(), "tcp", "provider.example:443")

	require.Nil(t, conn)
	require.ErrorContains(t, err, "non-public address")
	require.False(t, dialCalled)
}

func TestPublicOnlyDialRequiresPublicIPv4Candidate(t *testing.T) {
	t.Parallel()

	dialCalled := false
	dial := publicOnlyDialContext(
		func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("2606:4700:4700::1111")}, nil
		},
		func(context.Context, string, string) (net.Conn, error) {
			dialCalled = true
			return nil, errors.New("unexpected dial")
		},
	)

	conn, err := dial(t.Context(), "tcp", "provider.example:443")

	require.Nil(t, conn)
	require.ErrorContains(t, err, "no public IPv4 address")
	require.False(t, dialCalled)
}

func TestPublicOnlyDialValidatesAAAAButOnlyDialsPublicIPv4(t *testing.T) {
	t.Parallel()

	dialErr := errors.New("dial stopped by test")
	dial := publicOnlyDialContext(
		func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{
				netip.MustParseAddr("93.184.216.34"),
				netip.MustParseAddr("2606:4700:beef::a00:1"),
			}, nil
		},
		func(_ context.Context, network, address string) (net.Conn, error) {
			require.Equal(t, "tcp", network)
			require.Equal(t, "93.184.216.34:443", address)
			return nil, dialErr
		},
	)

	conn, err := dial(t.Context(), "tcp", "provider.example:443")

	require.Nil(t, conn)
	require.ErrorIs(t, err, dialErr)
}

func TestPublicOnlyDialRejectsMixedPublicAndPrivateDNS(t *testing.T) {
	t.Parallel()

	dialCalled := false
	dial := publicOnlyDialContext(
		func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{
				netip.MustParseAddr("93.184.216.34"),
				netip.MustParseAddr("10.0.0.1"),
			}, nil
		},
		func(context.Context, string, string) (net.Conn, error) {
			dialCalled = true
			return nil, errors.New("unexpected dial")
		},
	)

	conn, err := dial(t.Context(), "tcp", "provider.example:443")

	require.Nil(t, conn)
	require.ErrorContains(t, err, "non-public address")
	require.False(t, dialCalled)
}

func TestPublicOnlyDialRejectsZonedIPv6Addresses(t *testing.T) {
	t.Parallel()

	for _, address := range []string{
		"fd00::1%eth0",
		"2606:4700:4700::1111%eth0",
	} {
		t.Run(address, func(t *testing.T) {
			t.Parallel()

			dialCalled := false
			dial := publicOnlyDialContext(
				func(context.Context, string, string) ([]netip.Addr, error) {
					return nil, errors.New("literal address must not use DNS")
				},
				func(context.Context, string, string) (net.Conn, error) {
					dialCalled = true
					return nil, errors.New("unexpected dial")
				},
			)

			conn, err := dial(t.Context(), "tcp", "["+address+"]:443")

			require.Nil(t, conn)
			require.ErrorContains(t, err, "non-public address")
			require.False(t, dialCalled)
		})
	}
}

func TestPublicOnlyDialRejectsIPv6OutsideGlobalUnicastSpace(t *testing.T) {
	t.Parallel()

	for _, address := range []string{
		"::192.168.1.1",
		"400::1",
		"800::1",
		"1000::1",
	} {
		t.Run(address, func(t *testing.T) {
			t.Parallel()

			dialCalled := false
			dial := publicOnlyDialContext(
				func(context.Context, string, string) ([]netip.Addr, error) {
					return nil, errors.New("literal address must not use DNS")
				},
				func(context.Context, string, string) (net.Conn, error) {
					dialCalled = true
					return nil, errors.New("unexpected dial")
				},
			)

			conn, err := dial(t.Context(), "tcp", "["+address+"]:443")

			require.Nil(t, conn)
			require.ErrorContains(t, err, "non-public address")
			require.False(t, dialCalled)
		})
	}
}

func TestPublicOnlyDialResolvesOnceAndDialsValidatedIP(t *testing.T) {
	t.Parallel()

	lookupCount := 0
	dialCount := 0
	dialErr := errors.New("dial stopped by test")
	dial := publicOnlyDialContext(
		func(_ context.Context, network, host string) ([]netip.Addr, error) {
			lookupCount++
			require.Equal(t, "ip", network)
			require.Equal(t, "provider.example", host)

			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		func(_ context.Context, network, address string) (net.Conn, error) {
			dialCount++
			require.Equal(t, "tcp", network)
			require.Equal(t, "93.184.216.34:443", address)

			return nil, dialErr
		},
	)

	conn, err := dial(t.Context(), "tcp", "provider.example:443")

	require.Nil(t, conn)
	require.ErrorIs(t, err, dialErr)
	require.Equal(t, 1, lookupCount)
	require.Equal(t, 1, dialCount)
}

func TestPublicOnlyRedirectRejectsHTTPSDowngrade(t *testing.T) {
	t.Parallel()

	previous, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"https://provider.example/start",
		nil,
	)
	require.NoError(t, err)

	downgrade, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"http://cdn.example/video",
		nil,
	)
	require.NoError(t, err)
	require.ErrorContains(t, publicOnlyRedirectPolicy(downgrade, []*http.Request{previous}), "requires HTTPS")

	secure, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"https://cdn.example/video",
		nil,
	)
	require.NoError(t, err)
	require.NoError(t, publicOnlyRedirectPolicy(secure, []*http.Request{previous}))
}

func TestPublicOnlyHTTPClientRejectsPrivateTarget(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("private upstream must not receive a public-only request")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := LoadHTTPClientWithOutboundPolicyE(
		time.Second,
		"",
		true,
		OutboundPolicyPublicOnly,
	)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}

	require.Nil(t, resp)
	require.ErrorContains(t, err, "non-public address")
}

func TestPublicOnlyHTTPClientRejectsInitialHTTPURL(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("plain HTTP upstream must not receive a public-only request")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := LoadHTTPClientWithOutboundPolicyE(
		time.Second,
		"",
		false,
		OutboundPolicyPublicOnly,
	)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}

	require.Nil(t, resp)
	require.ErrorContains(t, err, "requires HTTPS")
}

func TestPublicOnlyHTTPClientRejectsProxy(t *testing.T) {
	t.Parallel()

	client, err := LoadHTTPClientWithOutboundPolicyE(
		time.Second,
		"http://127.0.0.1:7890",
		false,
		OutboundPolicyPublicOnly,
	)

	require.Nil(t, client)
	require.ErrorContains(t, err, "does not support proxies")
}

func TestPublicOnlyTransportDisablesEnvironmentProxy(t *testing.T) {
	t.Parallel()

	transport, err := createTransport(
		time.Second,
		"",
		false,
		OutboundPolicyPublicOnly,
	)
	require.NoError(t, err)
	require.Nil(t, transport.Proxy)
}

func TestDoRequestWithMetaAppliesChannelPublicOnlyPolicy(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("private upstream must not receive a public-only channel request")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	m := relaymeta.NewMeta(&coremodel.Channel{
		Configs:       coremodel.ChannelConfigs{"outbound_policy": "public_only"},
		SkipTLSVerify: true,
	}, 0, "", coremodel.ModelConfig{})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)

	resp, err := DoRequestWithMeta(req, m)
	if resp != nil {
		defer resp.Body.Close()
	}

	require.Nil(t, resp)
	require.ErrorContains(t, err, "non-public address")
}

func TestOutboundPolicyFromConfigsDefaultsToAllowPrivate(t *testing.T) {
	t.Parallel()

	require.Equal(t, OutboundPolicyAllowPrivate, OutboundPolicyFromConfigs(nil))
	require.Equal(
		t,
		OutboundPolicyPublicOnly,
		OutboundPolicyFromConfigs(map[string]any{"outbound_policy": "public_only"}),
	)
}
